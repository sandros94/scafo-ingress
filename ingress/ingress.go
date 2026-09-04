// Package ingress provides a Caddy dynamic upstreams source that resolves a
// request's backends from the fleet routing document a colocated scafo node
// daemon serves.
//
// The daemon knows which deployment runs on which node; the Caddy in front of
// it does not, and is never told in its own configuration. It holds nothing but
// a loopback endpoint URL and a bearer token, polls the daemon for the routing
// document, and proxies each request to whatever that document names for its
// host. A deployment that moves between nodes therefore costs no Caddy reload.
//
// The wire contract is the scafo ingress protocol version 1; see PROTOCOL.md
// for the document and the one security rule attached to it.
package ingress

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	stdpath "path"
	"strings"
	"sync/atomic"
	"time"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
	"go.uber.org/zap"

	"github.com/sandros94/scafo-ingress/internal/nodeapi"
)

// errPrefix is the name this module gives itself in the errors it returns. It
// is passed to the shared helpers in internal/nodeapi, which build errors on
// this module's behalf and must attribute them to it rather than to whichever
// scafo module happens to share the daemon.
const errPrefix = "scafo ingress"

// DocumentVersion is the routing document version this client understands. It
// is asserted against the document's own "v" member rather than announced in a
// request header: the daemon states which version it is serving, and a
// mismatch leaves the last understood document in place instead of routing
// against a shape this build cannot read.
const DocumentVersion = 1

// DefaultPollInterval is how long the poller waits between attempts when
// poll_interval is not configured.
const DefaultPollInterval = 5 * time.Second

// DefaultRequestTimeout bounds a single fetch of the routing document. The
// daemon is colocated, so anything slower than this is a fault rather than
// latency, and giving up returns the poller to its normal cadence.
const DefaultRequestTimeout = 10 * time.Second

// DefaultPrefer is the ordering the poller asks for when prefer is not
// configured: "self" puts the answering node's own upstreams first, which is
// what keeps a colocated Caddy proxying locally.
const DefaultPrefer = "self"

// PreferNone is the one prefer value this module reads rather than forwards.
// It is not a word the daemon knows: it suppresses the query parameter
// entirely, and an absent parameter is how the daemon is asked to leave the
// fleet's own order alone. Every other value -- "self", or the id of a node --
// goes over as written.
const PreferNone = "none"

// Outcome reports what a host and path resolved to. Its values are exactly
// what the module writes into the scafo.route request variable, so that an
// operator's handle_errors routes can tell the cases apart without this module
// choosing a status code for them.
type Outcome string

const (
	// OutcomeRoute means the document named upstreams for the request.
	OutcomeRoute Outcome = "ok"

	// OutcomeDenied means the host is a fleet API host and the path falls
	// under one of the daemon's denied prefixes, which the ingress must
	// answer itself.
	OutcomeDenied Outcome = "denied"

	// OutcomeUnknown means the document has no route and no API host for the
	// request's host.
	OutcomeUnknown Outcome = "unknown"

	// OutcomeUnavailable means there is no routing document to consult yet:
	// the daemon has not answered successfully since this module started.
	OutcomeUnavailable Outcome = "unavailable"
)

// Upstream is one backend as the daemon names it: a node and the address to
// dial on it.
type Upstream struct {
	Node    string `json:"node"`
	Address string `json:"address"`
}

// Route is one host the fleet serves and the ordered backends behind it.
//
// The order is a priority, never a weighting: the first upstream is the one to
// use, and the rest exist for when it fails. With prefer=self the answering
// node's own upstreams come first, so a colocated Caddy that honours the order
// stays on the local process and only leaves the node when it has to.
type Route struct {
	Host      string     `json:"host"`
	Project   string     `json:"project"`
	Env       string     `json:"env"`
	Upstreams []Upstream `json:"upstreams"`
}

// API describes the fleet API surface, when the answering node publishes it.
// It is absent from the document otherwise.
type API struct {
	// Hosts are the bare platform apexes routed to the daemon itself.
	Hosts []string `json:"hosts"`

	// Upstreams are the daemon's own addresses, ordered like a route's.
	Upstreams []Upstream `json:"upstreams"`

	// Denied are the path prefixes that must never be proxied on those
	// hosts. See Document.deniesPath.
	Denied []string `json:"denied"`
}

// Document is one fetch of the fleet routing document.
//
// A Document is immutable once index has run: every lookup reads the maps and
// slices built there and nothing writes to them again, which is what lets one
// value be shared by every request served from that generation.
type Document struct {
	Version int     `json:"v"`
	Routes  []Route `json:"routes"`
	API     *API    `json:"api,omitempty"`

	// hostUpstreams maps a normalized route host onto the upstreams to dial
	// for it, in document order.
	hostUpstreams map[string][]*reverseproxy.Upstream

	// apiHosts is the set of normalized fleet API hosts.
	apiHosts map[string]struct{}

	// apiUpstreams is the daemon's own upstreams, in document order.
	apiUpstreams []*reverseproxy.Upstream

	// deniedPrefixes are the API paths this module answers itself, with any
	// trailing separator removed so that matching is uniform.
	deniedPrefixes []string
}

// index derives the lookup structures from the decoded document. It is called
// once, on a document nothing else has seen yet, so that GetUpstreams is a map
// lookup and an already-built slice -- Caddy calls it on every request and
// every retry, and asks for it to be instantaneous.
func (d *Document) index() {
	d.hostUpstreams = make(map[string][]*reverseproxy.Upstream, len(d.Routes))
	for _, route := range d.Routes {
		host := NormalizeHost(route.Host)
		if host == "" {
			continue
		}
		// The document is sorted by host and holds each one once; if a
		// daemon repeats one anyway, the first wins rather than the last,
		// so the answer does not depend on how far the scan got.
		if _, taken := d.hostUpstreams[host]; taken {
			continue
		}
		// A route with no usable upstreams is kept, deliberately: the host
		// exists, it simply has nothing running behind it right now. Losing
		// it here would turn that into "no such host", which is a different
		// and wrong answer.
		d.hostUpstreams[host] = dialTargets(route.Upstreams)
	}

	if d.API == nil {
		return
	}
	d.apiHosts = make(map[string]struct{}, len(d.API.Hosts))
	for _, host := range d.API.Hosts {
		if host := NormalizeHost(host); host != "" {
			d.apiHosts[host] = struct{}{}
		}
	}
	d.apiUpstreams = dialTargets(d.API.Upstreams)
	for _, prefix := range d.API.Denied {
		prefix = strings.TrimSpace(prefix)
		if prefix == "" {
			continue
		}
		// A prefix is a path, so a trailing separator on it says nothing;
		// dropping it keeps "/v1/storage" and "/v1/storage/" the same rule.
		if trimmed := strings.TrimSuffix(prefix, "/"); trimmed != "" {
			prefix = trimmed
		}
		d.deniedPrefixes = append(d.deniedPrefixes, prefix)
	}
}

// Lookup resolves a normalized host and a request path against the document,
// returning the upstreams to proxy to and why.
//
// Routes are consulted before API hosts, so an application that has been given
// a name the platform also answers on keeps that name. The daemon's renderer
// already omits such apexes from the API section, but the precedence is stated
// here too rather than assumed of the peer.
//
// A nil Document -- no successful fetch yet -- resolves to OutcomeUnavailable
// for every request, which is a different thing from a document that has been
// read and does not mention the host.
func (d *Document) Lookup(host, path string) ([]*reverseproxy.Upstream, Outcome) {
	if d == nil {
		return nil, OutcomeUnavailable
	}
	if upstreams, ok := d.hostUpstreams[host]; ok {
		return upstreams, OutcomeRoute
	}
	if _, ok := d.apiHosts[host]; !ok {
		return nil, OutcomeUnknown
	}
	if d.deniesPath(path) {
		return nil, OutcomeDenied
	}
	return d.apiUpstreams, OutcomeRoute
}

// deniesPath reports whether path falls under one of the fleet API's denied
// prefixes. This is the security rule of that surface: those paths are the
// daemon's own control plane, they are reachable on loopback for the modules
// that hold a token, and they must never be reachable through the public
// ingress.
//
// Matching is at a path-segment boundary -- the prefix itself, or the prefix
// followed by a separator -- so /v1/storage refuses /v1/storage and
// /v1/storage/objects/x while leaving an application path like /v1/storagex
// alone.
//
// Both the path as written and its cleaned form are checked. net/http hands
// over r.URL.Path with its dot segments intact, and whether the daemon's own
// router collapses them before dispatching is not this module's to assume; a
// path that reaches a denied prefix under either reading is refused, because
// erring towards refusing is the only safe direction for this particular rule.
func (d *Document) deniesPath(path string) bool {
	if len(d.deniedPrefixes) == 0 {
		return false
	}
	if path == "" {
		path = "/"
	}
	cleaned := stdpath.Clean(path)
	for _, prefix := range d.deniedPrefixes {
		if coversPath(prefix, path) || coversPath(prefix, cleaned) {
			return true
		}
	}
	return false
}

// coversPath reports whether prefix matches path at a path-segment boundary.
func coversPath(prefix, path string) bool {
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	return len(path) == len(prefix) || path[len(prefix)] == '/'
}

// dialTargets turns the daemon's upstreams into Caddy's, preserving order and
// dropping entries with no address. The result is built once per document and
// handed to Caddy unchanged on every request; Caddy documents that it does not
// modify the slice it is given.
func dialTargets(upstreams []Upstream) []*reverseproxy.Upstream {
	targets := make([]*reverseproxy.Upstream, 0, len(upstreams))
	for _, upstream := range upstreams {
		address := strings.TrimSpace(upstream.Address)
		if address == "" {
			continue
		}
		targets = append(targets, &reverseproxy.Upstream{Dial: address})
	}
	return targets
}

// NormalizeHost reduces a host -- from a request or from the document -- to the
// form both sides are keyed by: no port, lowercase.
//
// The port is optional and SplitHostPort is only trusted when it succeeds, so a
// bare name, a name with a port and a bracketed address with a port all reduce
// correctly and anything it cannot parse is left whole rather than truncated.
func NormalizeHost(host string) string {
	host = strings.TrimSpace(host)
	if bare, _, err := net.SplitHostPort(host); err == nil {
		host = bare
	}
	return strings.ToLower(host)
}

// Routes is a live copy of the fleet routing document: it fetches the document
// from the scafo node daemon, keeps the last one that parsed, and answers
// lookups from it.
//
// A Routes value is safe for concurrent readers -- Document is an atomic load
// -- while fetching is single-threaded by construction: the first fetch happens
// before the poller is started, and the poller never overlaps itself. One
// Routes therefore serves every handler that wants the same document; see
// sharedRoutes in caddy.go.
type Routes struct {
	// base is the daemon's base URL with any trailing slash removed, so that
	// request paths can be appended directly.
	base string

	// token is the bearer token presented to the daemon.
	token string

	// prefer is the ordering asked of the daemon, sent verbatim as the prefer
	// query parameter. PreferNone and the empty string send no parameter at
	// all; see requestURL.
	prefer string

	// client performs the HTTP requests. It is dedicated to this Routes so
	// that timeouts and connection pooling are not shared with unrelated
	// traffic.
	client *http.Client

	// logger reports fetch failures and recoveries. It is replaceable, and
	// atomic, because a Routes outlives the configuration that created it: the
	// poller is shared between handlers and survives reloads, so each
	// configuration that adopts it lends it its own logger rather than leaving
	// it writing to the log of a configuration that has been unloaded. See
	// SetLogger.
	logger atomic.Pointer[zap.Logger]

	// current holds the last document that parsed, or nil before the first
	// one arrives. It is the only field a request reads.
	current atomic.Pointer[Document]

	// failing records whether the last fetch failed, so that a daemon that is
	// down logs once per state change instead of once per tick. Only the
	// fetch path touches it, and that path never runs concurrently with
	// itself.
	failing bool
}

// NewRoutes returns a Routes that polls the scafo node daemon at endpoint,
// authenticating with the given bearer token and asking for the given
// ordering.
//
// endpoint may include a path prefix (e.g. https://host/scafo); the protocol
// path is appended to it. A nil endpoint or an empty token is a programming
// error and yields a Routes whose every fetch fails. A nil logger is replaced
// by one that discards.
func NewRoutes(endpoint *url.URL, token, prefer string, logger *zap.Logger) *Routes {
	if logger == nil {
		logger = zap.NewNop()
	}
	routes := &Routes{
		base:   BaseURL(endpoint),
		token:  token,
		prefer: prefer,
		client: &http.Client{
			Timeout: DefaultRequestTimeout,
		},
	}
	routes.logger.Store(logger)
	return routes
}

// BaseURL reduces an endpoint to the form protocol paths are appended to: its
// string with any trailing separator removed.
//
// It is what a Routes is built on and what callers sharing one key it by, so
// that two endpoints differing only by a trailing slash are one conversation
// with the daemon rather than two.
func BaseURL(endpoint *url.URL) string {
	if endpoint == nil {
		return ""
	}
	return strings.TrimSuffix(endpoint.String(), "/")
}

// SetLogger hands the poller the logger to report fetch failures and
// recoveries to, replacing the one it holds. A nil logger is ignored, so a
// caller without one leaves the poller's own in place rather than silencing it.
//
// It exists because a shared poller has no single owner: it is provisioned by
// one configuration and may still be running under the next, and the logger it
// should be writing to is the one belonging to a configuration that is still
// loaded.
func (r *Routes) SetLogger(logger *zap.Logger) {
	if logger != nil {
		r.logger.Store(logger)
	}
}

// Document returns the last routing document that parsed, or nil when none has
// yet. Callers may hold onto it: a later fetch replaces the pointer rather than
// editing what a request is already reading.
func (r *Routes) Document() *Document {
	return r.current.Load()
}

// CloseIdleConnections drops connections to the daemon that are being kept
// alive but are not in use. It is meant for teardown; the Routes remains
// usable afterwards.
func (r *Routes) CloseIdleConnections() {
	if r.client != nil {
		r.client.CloseIdleConnections()
	}
}

// requestURL builds the routes URL, carrying the configured ordering as the
// prefer parameter unless there is none to ask for.
//
// PreferNone and an unset value both leave the parameter off rather than
// sending it empty, because the daemon reads an absent parameter as "the
// fleet's own order" -- so the way to ask for that is not to ask.
func (r *Routes) requestURL() string {
	target := r.base + "/v1/ingress/routes"
	if r.prefer == "" || r.prefer == PreferNone {
		return target
	}
	return target + "?" + url.Values{"prefer": []string{r.prefer}}.Encode()
}

// Fetch retrieves and parses the routing document without publishing it.
func (r *Routes) Fetch(ctx context.Context) (*Document, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.requestURL(), nil)
	if err != nil {
		return nil, fmt.Errorf("%s: building routes request: %w", errPrefix, err)
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Accept", "application/json")

	client := r.client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %s %s: %w", errPrefix, req.Method, req.URL.Path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer nodeapi.DrainAndClose(resp)
		return nil, nodeapi.NewAPIError(errPrefix, req, resp)
	}
	defer resp.Body.Close()

	var doc Document
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("%s: decoding routing document: %w", errPrefix, err)
	}
	if doc.Version != DocumentVersion {
		return nil, fmt.Errorf("%s: routing document version %d, want %d", errPrefix, doc.Version, DocumentVersion)
	}
	doc.index()
	return &doc, nil
}

// Refresh fetches the routing document and publishes it if it parsed, keeping
// the previous one otherwise. It reports nothing: a daemon that is briefly
// unreachable is an event to log and outlive, not a failure to propagate to
// whoever happened to call.
//
// Failures are logged once per state change rather than once per tick, so a
// daemon that stays down leaves one warning behind and a daemon that comes back
// says so.
func (r *Routes) Refresh(ctx context.Context) {
	fetchCtx, cancel := context.WithTimeout(ctx, DefaultRequestTimeout)
	defer cancel()

	doc, err := r.Fetch(fetchCtx)
	if err != nil {
		// A cancelled context is the process going away, not the daemon
		// failing; saying so on the way out would be noise.
		if ctx.Err() != nil {
			return
		}
		if !r.failing {
			r.failing = true
			logger := r.logger.Load()
			if r.current.Load() == nil {
				logger.Warn("fetching the routing document failed and none is known yet; requests will not be routed until the daemon answers",
					zap.Error(err))
			} else {
				logger.Warn("fetching the routing document failed; serving the last one that parsed",
					zap.Error(err))
			}
		}
		return
	}
	if r.failing {
		r.failing = false
		r.logger.Load().Info("the routing document is available again")
	}
	r.current.Store(doc)
}

// Run refreshes the routing document every interval until ctx is done.
//
// It waits interval between attempts rather than ticking on a schedule, so a
// slow fetch delays the next one instead of stacking up behind it.
func (r *Routes) Run(ctx context.Context, interval time.Duration) {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		r.Refresh(ctx)
		timer.Reset(interval)
	}
}
