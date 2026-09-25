package fleets

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
	"go.uber.org/zap"

	"github.com/sandros94/scafo-ingress/internal/nodeapi"
)

// nameRE is the shape of a fleet name: a slug, the same rule scafo applies to
// an application's name. It is what an operator types in a Caddyfile and what
// an environment variable maps onto, so one spelling is the whole vocabulary.
var nameRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ValidName reports whether name is a fleet name.
func ValidName(name string) bool {
	return nameRE.MatchString(name)
}

// Fleet is one scafo installation as an operator declares it: where its node
// daemon is, the token that opens it, and how this Caddy talks to it.
//
// Every other module in this repository refers to a fleet by name and holds
// no endpoint and no token of its own, so one credential is stated once and
// the modules that use it cannot drift apart.
type Fleet struct {
	// Name identifies the fleet. It is a slug, unique in the app.
	Name string `json:"name,omitempty"`

	// Config carries the endpoint and token options that reach the node
	// daemon. It is embedded, so they remain top-level members of a fleet's
	// JSON.
	nodeapi.Config

	// Prefer names whose upstreams should head every route, and is sent
	// verbatim as the prefer query parameter.
	//
	// "self", the default, means the node that answers the request -- which
	// is what keeps a colocated Caddy proxying to the process next to it.
	// Anything else is read by the daemon as a node id, and one that names no
	// node in the fleet simply has no effect. "none" is this package's own
	// word for asking nothing at all: it sends no parameter, and an absent
	// parameter is how the daemon is told to leave the fleet's own order
	// alone.
	Prefer string `json:"prefer,omitempty"`

	// PollInterval is how long to wait between fetches of this fleet's
	// routing document. Default: 5s. It bounds how stale a route may be.
	PollInterval caddy.Duration `json:"poll_interval,omitempty"`

	// Timeout bounds one fetch of the routing document. Default: 10s.
	Timeout caddy.Duration `json:"timeout,omitempty"`

	// StorageTimeout bounds one storage request to this fleet's daemon.
	// Default: 30s. It does not bound a lock acquisition, which blocks until
	// the lock is granted or the caller's context is done.
	StorageTimeout caddy.Duration `json:"storage_timeout,omitempty"`

	// HTTPRedirect decides whether this fleet's hosts are redirected from
	// plain HTTP to HTTPS. Unset means on, which is Caddy's own behaviour;
	// false serves this fleet's hosts over plain HTTP as well, and leaves
	// every other fleet alone.
	HTTPRedirect *bool `json:"http_redirect,omitempty"`

	endpoint *url.URL
	token    string
	routes   *Routes
	shared   *sharedRoutes
}

// EndpointURL returns the fleet's resolved node daemon URL. It is named apart
// from the Endpoint option it comes from because that option is a string an
// operator writes and this is what it parsed to.
func (f *Fleet) EndpointURL() *url.URL { return f.endpoint }

// BearerToken returns the fleet's resolved bearer token, whether it was given
// inline or read from a file.
func (f *Fleet) BearerToken() string { return f.token }

// Routes returns the fleet's poller.
func (f *Fleet) Routes() *Routes { return f.routes }

// Document returns the fleet's last routing document, or nil if none has
// arrived yet.
func (f *Fleet) Document() *Document { return f.routes.Document() }

// RedirectsHTTP reports whether this fleet's hosts are redirected from plain
// HTTP to HTTPS. It is on unless the operator turned it off.
func (f *Fleet) RedirectsHTTP() bool {
	return f.HTTPRedirect == nil || *f.HTTPRedirect
}

// StorageRequestTimeout is the bound on one storage request to this fleet.
func (f *Fleet) StorageRequestTimeout() time.Duration {
	if f.StorageTimeout > 0 {
		return time.Duration(f.StorageTimeout)
	}
	return DefaultStorageTimeout
}

// DefaultStorageTimeout bounds a single storage request to a node daemon.
const DefaultStorageTimeout = 30 * time.Second

// resolve expands placeholders, validates the fleet and fills in its defaults.
func (f *Fleet) resolve() error {
	if f.Name == "" {
		return fmt.Errorf("%s: a fleet must be named", errPrefix)
	}
	if !ValidName(f.Name) {
		return fmt.Errorf("%s: fleet name %q is not a slug (lowercase alphanumerics separated by single hyphens)", errPrefix, f.Name)
	}

	prefix := fmt.Sprintf("%s: fleet %s", errPrefix, f.Name)
	endpoint, token, err := f.Config.Resolve(prefix)
	if err != nil {
		return err
	}
	f.endpoint = endpoint
	f.token = token

	f.Prefer = strings.TrimSpace(caddy.NewReplacer().ReplaceKnown(f.Prefer, ""))
	if f.Prefer == "" {
		f.Prefer = DefaultPrefer
	}

	if f.PollInterval < 0 {
		return fmt.Errorf("%s: poll_interval must be positive, got %s", prefix, time.Duration(f.PollInterval))
	}
	if f.PollInterval == 0 {
		f.PollInterval = caddy.Duration(DefaultPollInterval)
	}
	if f.Timeout < 0 {
		return fmt.Errorf("%s: timeout must be positive, got %s", prefix, time.Duration(f.Timeout))
	}
	if f.Timeout == 0 {
		f.Timeout = caddy.Duration(DefaultRequestTimeout)
	}
	if f.StorageTimeout < 0 {
		return fmt.Errorf("%s: storage_timeout must be positive, got %s", prefix, time.Duration(f.StorageTimeout))
	}
	if f.StorageTimeout == 0 {
		f.StorageTimeout = caddy.Duration(DefaultStorageTimeout)
	}
	return nil
}

// Match is the answer to "who serves this request": which fleet, on which
// upstreams, and what the document key that answered was.
type Match struct {
	// Fleet is the name of the fleet that owns the host, empty when none
	// does.
	Fleet string

	// Upstreams are the backends to dial, in the document's priority order.
	Upstreams []*reverseproxy.Upstream

	// Outcome is why: ok, denied, unknown or unavailable.
	Outcome Outcome

	// Matched is the document key that answered: a route host, a wildcard
	// pattern such as "*.example.com", or a fleet API host. It is empty when
	// nothing matched.
	Matched string
}

// kind distinguishes the three keyspaces a host can be claimed in. They are
// kept apart because "example.com as a route" and "example.com as a fleet API
// apex" are different claims that different fleets may legitimately hold.
type kind uint8

const (
	kindRoute kind = iota
	kindAPI
	kindWildcard
)

func (k kind) String() string {
	switch k {
	case kindRoute:
		return "route"
	case kindAPI:
		return "api"
	default:
		return "wildcard"
	}
}

// claimKey names one thing a fleet can own.
type claimKey struct {
	kind kind
	name string // a host, or a wildcard's base
}

func (c claimKey) String() string {
	if c.kind == kindWildcard {
		return WildcardPrefix + c.name
	}
	return c.name
}

// index is the cross-fleet view a request is answered from. It is built once
// per real change and replaced whole, so a reader holds a consistent snapshot
// without a lock.
type index struct {
	// generations is each fleet's document generation at the moment this
	// index was built, in app fleet order. A reader compares it against the
	// live counters to find out whether the index is still current.
	generations []uint64

	// routes, apis and wilds map a claimed name onto the one fleet that owns
	// it and what to do about it.
	routes map[string]resolved
	apis   map[string]resolved
	wilds  map[string]resolved

	// documents reports whether at least one fleet had a document at all. An
	// index built from nothing answers "unavailable" rather than "unknown".
	documents bool
}

// resolved is one claimed name's answer.
type resolved struct {
	fleet     *Fleet
	doc       *Document
	upstreams []*reverseproxy.Upstream
}

// Resolve answers which fleet serves host, and with which upstreams.
//
// Three things are consulted, in this order, and the order is the contract:
//
//  1. a route naming the host exactly, which therefore wins even when it has
//     no upstreams at all -- "this host exists and has nothing running" is an
//     answer, and falling through to a wildcard would replace it with another
//     application's process;
//  2. a fleet API host naming it exactly, so that an application route keeps a
//     name the platform also answers on while a wildcard never takes one away
//     from the platform;
//  3. a wildcard route covering it, which is the host with its first label
//     replaced by "*" and looked up once.
//
// Each step consults every fleet at once: the claim on a name decides which
// fleet answers for it, and an exact claim in any fleet beats a wildcard claim
// in any other.
func (a *App) Resolve(host, path string) Match {
	idx := a.currentIndex()
	if r, ok := idx.routes[host]; ok {
		return Match{Fleet: r.fleet.Name, Upstreams: r.upstreams, Outcome: OutcomeRoute, Matched: host}
	}
	if r, ok := idx.apis[host]; ok {
		if r.doc.DeniesPath(path) {
			return Match{Fleet: r.fleet.Name, Outcome: OutcomeDenied, Matched: host}
		}
		return Match{Fleet: r.fleet.Name, Upstreams: r.upstreams, Outcome: OutcomeRoute, Matched: host}
	}
	if base, ok := WildcardCover(host); ok {
		if r, ok := idx.wilds[base]; ok {
			return Match{Fleet: r.fleet.Name, Upstreams: r.upstreams, Outcome: OutcomeRoute, Matched: WildcardPrefix + base}
		}
	}
	if !idx.documents {
		return Match{Outcome: OutcomeUnavailable}
	}
	return Match{Outcome: OutcomeUnknown}
}

// Owner reports which fleet owns a name for the purpose of its certificate:
// the same resolution a request gets, minus the path rules.
//
// A wildcard subject -- "*.example.com" -- is owned by the fleet that carries
// that wildcard: either as a route, as a base it asked a certificate for, or
// as its platform apex. Every other name is owned by whichever fleet would
// answer a request for it.
func (a *App) Owner(name string) (*Fleet, bool) {
	name = NormalizeHost(name)
	if base, ok := WildcardBase(name); ok {
		return a.wildcardOwner(base)
	}
	idx := a.currentIndex()
	if r, ok := idx.routes[name]; ok {
		return r.fleet, true
	}
	if r, ok := idx.apis[name]; ok {
		return r.fleet, true
	}
	if base, ok := WildcardCover(name); ok {
		if r, ok := idx.wilds[base]; ok {
			return r.fleet, true
		}
	}
	return nil, false
}

// wildcardOwner finds the fleet a "*.base" certificate belongs to. A wildcard
// route for the base is the first answer; the base being one the fleet asked a
// certificate for, or its own platform apex, is the second — those are the
// bases §4 manages, and their certificate has to live somewhere.
func (a *App) wildcardOwner(base string) (*Fleet, bool) {
	idx := a.currentIndex()
	if r, ok := idx.wilds[base]; ok {
		return r.fleet, true
	}
	if r, ok := idx.apis[base]; ok {
		return r.fleet, true
	}
	for _, fleet := range a.fleets {
		doc := fleet.Document()
		for _, want := range doc.WildcardBases() {
			if want == base {
				return fleet, true
			}
		}
	}
	return nil, false
}

// CertificateAllowed reports whether a certificate may be obtained on demand
// for name, and which fleet asked for it.
//
// Only an EXACT route host or an exact fleet API host qualifies. A name that
// is merely covered by a "*." entry does NOT: an on-demand certificate per
// arbitrary subdomain lets anyone who can point a name at this ingress burn
// a customer's weekly issuance limit at the CA, one handshake at a time,
// while the wildcard those names are covered by is issued once through DNS-01
// and costs nothing per name. So the wildcard is managed (see wildcards.go)
// and the on-demand door stays shut.
func (a *App) CertificateAllowed(name string) (string, bool) {
	name = NormalizeHost(name)
	idx := a.currentIndex()
	if r, ok := idx.routes[name]; ok {
		return r.fleet.Name, true
	}
	if r, ok := idx.apis[name]; ok {
		return r.fleet.Name, true
	}
	return "", false
}

// PlainHTTP reports whether host belongs to a fleet that serves it over plain
// HTTP rather than redirecting to HTTPS. A host no fleet owns is not plain: an
// unknown name gets the default, which is the redirect.
func (a *App) PlainHTTP(host string) bool {
	host = NormalizeHost(host)
	idx := a.currentIndex()
	if r, ok := idx.routes[host]; ok {
		return !r.fleet.RedirectsHTTP()
	}
	if r, ok := idx.apis[host]; ok {
		return !r.fleet.RedirectsHTTP()
	}
	if base, ok := WildcardCover(host); ok {
		if r, ok := idx.wilds[base]; ok {
			return !r.fleet.RedirectsHTTP()
		}
	}
	return false
}

// Fleets returns the app's fleets in declaration order.
func (a *App) Fleets() []*Fleet { return a.fleets }

// Fleet returns the fleet of that name.
func (a *App) Fleet(name string) (*Fleet, bool) {
	fleet, ok := a.byName[name]
	return fleet, ok
}

// currentIndex returns the cross-fleet view, rebuilding it first when any
// fleet's document has changed since the last build.
//
// The check is one atomic load per fleet and happens on the request path; the
// rebuild happens on the first request after a change, under a mutex, and at
// most once per change. Rebuilding lazily rather than in the poller is what
// keeps the poller's callback cheap and keeps a burst of fleets that all
// changed at once from rebuilding the index once each.
func (a *App) currentIndex() *index {
	idx := a.index.Load()
	if idx != nil && a.indexCurrent(idx) {
		return idx
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	// Another goroutine may have rebuilt it while this one waited.
	if idx := a.index.Load(); idx != nil && a.indexCurrent(idx) {
		return idx
	}
	rebuilt := a.rebuild()
	a.index.Store(rebuilt)
	return rebuilt
}

// indexCurrent reports whether idx was built from the documents the fleets
// hold right now.
func (a *App) indexCurrent(idx *index) bool {
	if len(idx.generations) != len(a.fleets) {
		return false
	}
	for i, fleet := range a.fleets {
		if fleet.routes.Generation() != idx.generations[i] {
			return false
		}
	}
	return true
}

// rebuild derives the cross-fleet view from every fleet's current document.
// The caller holds a.mu.
func (a *App) rebuild() *index {
	idx := &index{
		generations: make([]uint64, len(a.fleets)),
		routes:      map[string]resolved{},
		apis:        map[string]resolved{},
		wilds:       map[string]resolved{},
	}

	// candidates collects, per claimed name, every fleet publishing it, in
	// declaration order.
	candidates := map[claimKey][]*Fleet{}
	docs := map[*Fleet]*Document{}

	for i, fleet := range a.fleets {
		idx.generations[i] = fleet.routes.Generation()
		doc := fleet.Document()
		docs[fleet] = doc
		if doc == nil {
			continue
		}
		idx.documents = true
		for _, host := range doc.RouteHosts() {
			key := claimKey{kindRoute, host}
			candidates[key] = append(candidates[key], fleet)
		}
		for _, host := range doc.APIHosts() {
			key := claimKey{kindAPI, host}
			candidates[key] = append(candidates[key], fleet)
		}
		for _, base := range doc.WildcardRouteBases() {
			key := claimKey{kindWildcard, base}
			candidates[key] = append(candidates[key], fleet)
		}
	}

	// A claim is sticky: the fleet that published a name FIRST keeps it for as
	// long as it goes on publishing it, whatever else appears later. That is
	// what keeps a name from flapping between two fleets that both claim it,
	// and what makes the conflict a thing to fix rather than a thing to watch
	// oscillate. A name claimed by two fleets in the very same rebuild has no
	// "first", so declaration order decides and the conflict is logged.
	for key, holders := range candidates {
		winner := holders[0]
		if previous, ok := a.claims[key]; ok {
			for _, holder := range holders {
				if holder.Name == previous {
					winner = holder
					break
				}
			}
		}
		a.claims[key] = winner.Name

		entry := resolved{fleet: winner, doc: docs[winner]}
		switch key.kind {
		case kindRoute:
			entry.upstreams, _ = entry.doc.RouteUpstreams(key.name)
			idx.routes[key.name] = entry
		case kindAPI:
			entry.upstreams, _ = entry.doc.APIUpstreams(key.name)
			idx.apis[key.name] = entry
		case kindWildcard:
			entry.upstreams, _ = entry.doc.WildcardUpstreams(key.name)
			idx.wilds[key.name] = entry
		}
	}

	// A claim whose holder stopped publishing the name is forgotten, so the
	// map cannot grow without bound and a name that comes back is claimed
	// afresh.
	for key := range a.claims {
		if _, ok := candidates[key]; !ok {
			delete(a.claims, key)
		}
	}

	a.logConflicts(candidates)
	return idx
}

// logConflicts reports names more than one fleet publishes, once per change of
// the conflict rather than once per rebuild — and a rebuild only happens when
// a document changed, so a standing conflict leaves one line behind and a
// resolved one says so.
func (a *App) logConflicts(candidates map[claimKey][]*Fleet) {
	seen := map[claimKey]string{}
	for key, holders := range candidates {
		if len(holders) < 2 {
			continue
		}
		names := make([]string, 0, len(holders))
		for _, holder := range holders {
			names = append(names, holder.Name)
		}
		sort.Strings(names)
		seen[key] = a.claims[key] + " over " + strings.Join(names, ",")
	}

	for key, state := range seen {
		if a.conflicts[key] == state {
			continue
		}
		a.logger.Warn("two fleets publish the same host; the one that published it first keeps it",
			zap.String("host", key.String()),
			zap.String("kind", key.kind.String()),
			zap.String("served_by", a.claims[key]),
			zap.String("claimed_by", state))
	}
	for key := range a.conflicts {
		if _, ok := seen[key]; !ok {
			a.logger.Info("a host is no longer claimed by more than one fleet",
				zap.String("host", key.String()),
				zap.String("kind", key.kind.String()))
		}
	}
	a.conflicts = seen
}

// invalidate is what a poller calls when its document changed. It does not
// rebuild: the next reader does, at most once however many fleets changed.
func (a *App) invalidate() {
	if a.wild != nil {
		a.wild.notify()
	}
}

// sortedNames returns the members of a set in a stable order, for log lines
// and for the wildcard manager's comparisons.
func sortedNames(set map[string]struct{}) []string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
