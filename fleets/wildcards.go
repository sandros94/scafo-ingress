package fleets

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/modules/caddytls"
	"github.com/caddyserver/certmagic"
	"go.uber.org/zap"
)

// A fleet's routing document may carry a `wildcards` member: the bases it
// wants a `*.<base>` certificate for -- its platform domains and every custom
// domain claimed together with its subdomains. This file is what turns that
// list into managed certificates.
//
// The reason it exists is the on-demand rule in CertificateAllowed. A name
// covered only by a wildcard is NOT allowed to trigger an issuance of its own,
// because that would let anyone pointing names at this ingress spend a
// customer's weekly issuance limit at the CA. The wildcard has to come from
// somewhere else, and DNS-01 is that somewhere: one certificate per base,
// obtained without an inbound request, covering every name under it.
//
// Two shapes of zone reach the provider. A base inside one of the operator's
// own zones is solved there directly. A base that is a customer's domain is
// solved through the DELEGATION: the customer creates
// `_acme-challenge.<base> CNAME <acme_delegation>` once, and every challenge
// for that base is then written at that ONE fixed name in the operator's zone.

// wildcardRetryMin and wildcardRetryMax bound the backoff between attempts at
// a base whose issuance failed. Issuance itself is retried by CertMagic on its
// own schedule; this backoff is for the call that hands it the names, which
// fails only when something is configured wrong or the TLS app refused.
const (
	wildcardRetryMin = 30 * time.Second
	wildcardRetryMax = 15 * time.Minute
)

// wildcardDepth is how many labels a zone's automation policy covers beneath
// itself.
//
// CertMagic decides whether an automation policy applies to a subject with
// MatchWildcard, which replaces the subject's labels with "*" one at a time,
// from the left, cumulatively. So `*.example.com` as a policy subject matches
// `a.example.com` and `*.example.com`, and matches NOTHING deeper:
// `*.shop.example.com` only ever produces the candidates `*.shop.example.com`,
// `*.*.example.com`, `*.*.*.com`, `*.*.*.*`. There is no pattern that means
// "anything under this zone", so a zone contributes one pattern per depth, and
// this is how many. Four covers `*.a.b.c.d.<zone>`, which is deeper than any
// platform or customer domain in practice.
const wildcardDepth = 4

// wildcardManager keeps the certificates for the bases the fleets ask for.
type wildcardManager struct {
	app *App

	// tls is Caddy's TLS app, which owns the certificate cache a handshake
	// reads. Nil when no TLS app is configured, which is a Caddy serving no
	// HTTPS at all.
	tls *caddytls.TLS

	// policies are the automation policies this manager installed, by group.
	// They are installed once, during Start, and never edited afterwards:
	// Caddy reads the policy list on handshake paths without a lock, so
	// changing it under a running server is not something a module may do.
	policies map[groupKey]*caddytls.AutomationPolicy

	// managed is every subject handed to the TLS app so far.
	managed map[string]struct{}

	// failing is the subjects the last attempt could not manage, and the
	// reason, so that a failure is logged once per state change rather than
	// once per poll.
	failing map[string]string

	// uncovered is the bases no installed policy can take, and which
	// therefore wait for a reload. Logged once per state change, same as a
	// failure.
	uncovered map[string]struct{}

	wake chan struct{}
	done chan struct{}
	once sync.Once
}

// groupKey identifies one automation policy: the provider that solves the
// challenge, and the name the challenge record is written at when it is
// delegated.
type groupKey struct {
	provider string
	override string
}

func newWildcardManager(app *App) *wildcardManager {
	return &wildcardManager{
		app:       app,
		policies:  map[groupKey]*caddytls.AutomationPolicy{},
		managed:   map[string]struct{}{},
		failing:   map[string]string{},
		uncovered: map[string]struct{}{},
		wake:      make(chan struct{}, 1),
		done:      make(chan struct{}),
	}
}

// notify tells the manager that a routing document changed. It never blocks:
// one pending wake-up is as good as ten.
func (w *wildcardManager) notify() {
	if w == nil {
		return
	}
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// stop ends the manager's goroutine.
func (w *wildcardManager) stop() {
	w.once.Do(func() { close(w.done) })
}

// start installs the automation policies and begins following the documents.
//
// With no DNS provider configured nothing is managed and the behaviour is
// exactly what it is without this file: every certificate is obtained on
// demand, for an exact host, and a wildcard base is simply not covered.
func (w *wildcardManager) start() error {
	app := w.app
	if !app.hasDNS() {
		app.logger.Debug("no DNS provider is configured; wildcard certificates are not managed")
		return nil
	}

	value, err := app.ctx.AppIfConfigured("tls")
	if err != nil {
		if errors.Is(err, caddy.ErrNotConfigured) {
			app.logger.Debug("no TLS app is configured; wildcard certificates are not managed")
			return nil
		}
		return fmt.Errorf("%s: reaching the tls app: %v", errPrefix, err)
	}
	tlsApp, ok := value.(*caddytls.TLS)
	if !ok {
		return fmt.Errorf("%s: the tls app is %T", errPrefix, value)
	}
	w.tls = tlsApp

	if err := w.installPolicies(); err != nil {
		return err
	}

	w.reconcile()
	go w.run()
	return nil
}

// run follows the documents. A change wakes it; otherwise it retries what is
// failing on a backoff.
func (w *wildcardManager) run() {
	backoff := wildcardRetryMin
	timer := time.NewTimer(backoff)
	defer timer.Stop()
	for {
		select {
		case <-w.done:
			return
		case <-w.wake:
			w.reconcile()
			backoff = wildcardRetryMin
		case <-timer.C:
			if len(w.failing) > 0 || len(w.uncovered) > 0 {
				w.reconcile()
				if backoff *= 2; backoff > wildcardRetryMax {
					backoff = wildcardRetryMax
				}
			} else {
				backoff = wildcardRetryMin
			}
		}
		timer.Reset(backoff)
	}
}

// plan is what the current documents ask for: every subject, grouped by the
// policy that would obtain it.
type plan struct {
	// byGroup maps a policy group onto its subjects.
	byGroup map[groupKey]map[string]struct{}

	// origin remembers which base each subject came from, for log lines.
	origin map[string]string

	// unsolvable are the bases no provider and no delegation can solve.
	unsolvable map[string]string
}

// planSubjects reads every fleet's document and works out what to manage.
func (a *App) planSubjects() plan {
	p := plan{
		byGroup:    map[groupKey]map[string]struct{}{},
		origin:     map[string]string{},
		unsolvable: map[string]string{},
	}

	// A base that is also a fleet API apex gets the bare name as well as the
	// wildcard: `*.example.com` does not cover `example.com`, and the platform
	// answers on both.
	apexes := map[string]struct{}{}
	for _, fleet := range a.fleets {
		for _, host := range fleet.Document().APIHosts() {
			apexes[host] = struct{}{}
		}
	}

	for _, fleet := range a.fleets {
		for _, base := range fleet.Document().WildcardBases() {
			provider, override, ok := a.solverFor(base)
			if !ok {
				p.unsolvable[base] = "no DNS provider holds a zone for it and no ACME delegation is configured"
				continue
			}
			key := groupKey{provider: provider.Name, override: override}
			subjects, seen := p.byGroup[key]
			if !seen {
				subjects = map[string]struct{}{}
				p.byGroup[key] = subjects
			}
			wildcard := WildcardPrefix + base
			subjects[wildcard] = struct{}{}
			p.origin[wildcard] = base
			if _, isApex := apexes[base]; isApex {
				subjects[base] = struct{}{}
				p.origin[base] = base
			}
		}
	}
	return p
}

// solverFor picks how a base's DNS challenge is solved: with which provider,
// and at which name.
//
// A base inside a declared zone is solved in that zone, by the provider that
// holds it. A base inside none of them is solved through the delegation, in
// whichever zone the delegation name itself lives in -- that is the whole
// point of the delegation: the customer's zone is never written to.
//
// When NO zone is declared anywhere, the default provider is the operator's
// only statement of where their DNS lives, so it takes everything and the
// delegation stays out of the way. Listing zones is therefore what turns the
// delegation on for the names that are not yours.
func (a *App) solverFor(base string) (*DNSProvider, string, bool) {
	var zoned *DNSProvider
	zonedLen := 0
	anyZones := false
	for i := range a.DNS {
		provider := &a.DNS[i]
		if len(provider.Zones) == 0 {
			continue
		}
		anyZones = true
		if length, ok := provider.covers(base); ok && length > zonedLen {
			zoned, zonedLen = provider, length
		}
	}
	if zoned != nil {
		return zoned, "", true
	}

	var fallback *DNSProvider
	for i := range a.DNS {
		if provider := &a.DNS[i]; len(provider.Zones) == 0 {
			fallback = provider
			break
		}
	}

	if anyZones && a.ACMEDelegation != "" {
		// The record is written at the delegation name, so it is the
		// delegation name -- not the base -- that decides whose provider does
		// the writing.
		if provider, ok := a.providerFor(a.ACMEDelegation); ok {
			return provider, a.ACMEDelegation, true
		}
		return nil, "", false
	}
	if fallback != nil {
		return fallback, "", true
	}
	// Every zone is spoken for, none of them covers this base, and there is no
	// default provider to fall back on. With no delegation either, nothing can
	// solve it -- which planSubjects reports rather than guesses at.
	return nil, "", false
}

// installPolicies adds one automation policy per group to the TLS app.
//
// They are installed ONCE, here, while Caddy is starting and nothing is
// serving yet, because the TLS app reads its policy list on handshake paths
// without a lock: adding one later would be a data race, whatever it bought.
// What a policy covers is therefore fixed at this moment, and covering a base
// that appears later is the job of the zone patterns -- see wildcardDepth.
func (w *wildcardManager) installPolicies() error {
	app := w.app
	current := app.planSubjects()

	// Every group that could ever exist, not only the ones with subjects
	// right now: one per provider, plus one per provider for the delegated
	// names it writes.
	groups := map[groupKey]struct{}{}
	for key := range current.byGroup {
		groups[key] = struct{}{}
	}
	for i := range app.DNS {
		groups[groupKey{provider: app.DNS[i].Name}] = struct{}{}
	}
	if app.ACMEDelegation != "" {
		if provider, ok := app.providerFor(app.ACMEDelegation); ok {
			groups[groupKey{provider: provider.Name, override: app.ACMEDelegation}] = struct{}{}
		}
	}

	keys := make([]groupKey, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].provider != keys[j].provider {
			return keys[i].provider < keys[j].provider
		}
		return keys[i].override < keys[j].override
	})

	for _, key := range keys {
		provider, ok := app.providerByName(key.provider)
		if !ok {
			continue
		}
		subjects := map[string]struct{}{}
		for subject := range current.byGroup[key] {
			subjects[subject] = struct{}{}
		}
		// A group that writes in its own zones covers everything under them,
		// so a base that appears later needs no new policy. A delegated group
		// has no zone to derive from -- the names are customers' -- so it
		// covers exactly what was known at startup.
		if key.override == "" {
			for _, zone := range provider.Zones {
				for _, pattern := range zonePatterns(zone) {
					subjects[pattern] = struct{}{}
				}
			}
		}
		if len(subjects) == 0 {
			continue
		}

		policy := &caddytls.AutomationPolicy{
			SubjectsRaw: sortedNames(subjects),
			IssuersRaw:  []json.RawMessage{w.issuerJSON(provider, key.override)},
		}
		if err := w.tls.AddAutomationPolicy(policy); err != nil {
			return fmt.Errorf("%s: adding the automation policy for the %s DNS provider: %v", errPrefix, provider.describe(), err)
		}
		w.policies[key] = policy
		app.logger.Info("managing wildcard certificates through a DNS provider",
			zap.String("provider", provider.Module),
			zap.String("provider_name", provider.describe()),
			zap.String("acme_delegation", key.override),
			zap.Strings("covers", policy.SubjectsRaw))
	}
	return nil
}

// providerByName finds a configured provider by its name; the default
// provider's name is empty.
func (a *App) providerByName(name string) (*DNSProvider, bool) {
	for i := range a.DNS {
		if a.DNS[i].Name == name {
			return &a.DNS[i], true
		}
	}
	return nil, false
}

// issuerJSON builds the ACME issuer for one group: the DNS-01 challenge
// through that provider, and nothing else. HTTP and TLS-ALPN are not disabled
// explicitly because CertMagic already offers only the DNS challenge once a
// DNS solver is configured -- and neither of the other two can prove a
// wildcard anyway.
func (w *wildcardManager) issuerJSON(provider *DNSProvider, override string) json.RawMessage {
	issuer := &caddytls.ACMEIssuer{
		CA:    w.app.ACMECA,
		Email: w.app.ACMEEmail,
		Challenges: &caddytls.ChallengesConfig{
			DNS: &caddytls.DNSChallengeConfig{
				ProviderRaw:    provider.ConfigRaw,
				OverrideDomain: override,
			},
		},
	}
	return caddyconfig.JSONModuleObject(issuer, "module", "acme", nil)
}

// zonePatterns spells the automation-policy subjects that cover a zone: the
// apex itself, and one wildcard per depth beneath it. See wildcardDepth for
// why a single pattern will not do.
func zonePatterns(zone string) []string {
	patterns := []string{zone}
	stars := ""
	for i := 0; i < wildcardDepth; i++ {
		stars += "*."
		patterns = append(patterns, stars+zone)
	}
	return patterns
}

// reconcile hands the TLS app every subject the documents currently ask for
// and that an installed policy can take.
func (w *wildcardManager) reconcile() {
	if w.tls == nil {
		return
	}
	current := w.app.planSubjects()

	wanted := map[string]struct{}{}
	uncovered := map[string]struct{}{}
	for key, subjects := range current.byGroup {
		policy, installed := w.policies[key]
		for subject := range subjects {
			if _, done := w.managed[subject]; done {
				continue
			}
			if !installed || !policyCovers(policy, subject) {
				uncovered[current.origin[subject]] = struct{}{}
				continue
			}
			wanted[subject] = struct{}{}
		}
	}
	for base := range current.unsolvable {
		uncovered[base] = struct{}{}
	}

	failing := map[string]string{}
	if len(wanted) > 0 {
		// Manage is handed the whole batch at once: it bins the names by
		// policy itself, and one call means one certmagic.Config rather than
		// one per name.
		if err := w.tls.Manage(wanted); err != nil {
			for subject := range wanted {
				failing[subject] = err.Error()
			}
		} else {
			for subject := range wanted {
				w.managed[subject] = struct{}{}
			}
		}
	}

	w.report(current, failing, uncovered)
	w.failing = failing
	w.uncovered = uncovered
}

// policyCovers reports whether a policy's subject filter admits subject, by
// the same rule the TLS app applies.
func policyCovers(policy *caddytls.AutomationPolicy, subject string) bool {
	for _, filter := range policy.SubjectsRaw {
		if certmagic.MatchWildcard(subject, filter) {
			return true
		}
	}
	return false
}

// report logs what changed, once per state change and never once per poll.
func (w *wildcardManager) report(current plan, failing map[string]string, uncovered map[string]struct{}) {
	logger := w.app.logger
	for subject, reason := range failing {
		if w.failing[subject] == reason {
			continue
		}
		logger.Warn("a wildcard certificate could not be managed; retrying",
			zap.String("subject", subject),
			zap.String("error", reason))
	}
	for subject := range w.failing {
		if _, still := failing[subject]; !still {
			logger.Info("a wildcard certificate is being managed again", zap.String("subject", subject))
		}
	}

	for base := range uncovered {
		if _, known := w.uncovered[base]; known {
			continue
		}
		fields := []zap.Field{zap.String("base", base)}
		if reason, ok := current.unsolvable[base]; ok {
			fields = append(fields, zap.String("reason", reason))
		} else {
			fields = append(fields,
				zap.String("reason", "no automation policy installed at startup covers it"),
				zap.String("fix", "list the base's zone under its DNS provider, or reload Caddy"))
		}
		if w.app.ACMEDelegation != "" {
			fields = append(fields, zap.String("delegation_record",
				DelegationRecord(base, w.app.ACMEDelegation)))
		}
		logger.Warn("a fleet asked for a wildcard certificate this ingress cannot obtain", fields...)
	}
	for base := range w.uncovered {
		if _, still := uncovered[base]; !still {
			logger.Info("a wildcard base is covered again", zap.String("base", base))
		}
	}
}

// DelegationRecord spells the DNS record a customer creates so that a base of
// theirs can be solved in the operator's zone. It is one sentence, written
// once, so the log line and the documentation cannot drift apart.
func DelegationRecord(base, delegation string) string {
	return "_acme-challenge." + base + " CNAME " + delegation
}
