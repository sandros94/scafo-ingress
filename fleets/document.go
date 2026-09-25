// Package fleets holds the scafo Caddy app: the fleets an operator declares,
// the poller each one gets, and the single view of "which fleet serves this
// name" that every other module in this repository reads.
//
// One scafo fleet is one installation: one node daemon endpoint, one bearer
// token, one routing document. One Caddy may stand in front of several of them
// at once — an agency running a fleet per client on the same machines — and the
// single-fleet case is the same code with one entry in the list.
//
// The wire contract is the scafo ingress protocol version 1; see PROTOCOL.md
// for the document and the rules attached to it.
package fleets

import (
	"hash/fnv"
	"net"
	stdpath "path"
	"strings"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
)

// errPrefix is the name this package gives itself in the errors it returns.
const errPrefix = "scafo"

// DocumentVersion is the routing document version this client understands. It
// is asserted against the document's own "v" member rather than announced in a
// request header: the daemon states which version it is serving, and a
// mismatch leaves the last understood document in place instead of routing
// against a shape this build cannot read.
const DocumentVersion = 1

// Outcome reports what a host and path resolved to. Its values are exactly
// what the dynamic upstreams module writes into the scafo.route request
// variable, so that an operator's handle_errors routes can tell the cases
// apart without a module choosing a status code for them.
type Outcome string

const (
	// OutcomeRoute means some fleet's document named upstreams for the request.
	OutcomeRoute Outcome = "ok"

	// OutcomeDenied means the host is a fleet API host and the path falls
	// under one of that daemon's denied prefixes, which the ingress must
	// answer itself.
	OutcomeDenied Outcome = "denied"

	// OutcomeUnknown means no fleet has a route and no API host for the
	// request's host.
	OutcomeUnknown Outcome = "unknown"

	// OutcomeUnavailable means there is no routing document to consult at all:
	// not one fleet has answered successfully since this module started. It is
	// deliberately not "unknown" — a cold start is not a missing deployment.
	OutcomeUnavailable Outcome = "unavailable"
)

// Upstream is one backend as the daemon names it: a node and the address to
// dial on it.
type Upstream struct {
	Node    string `json:"node"`
	Address string `json:"address"`
}

// WildcardPrefix is the one label a route host may carry in place of a name:
// a host of the form "*.example.com" covers every single-label name under
// example.com. It is the only form of wildcard the document may use -- one
// leading "*." and no other asterisk anywhere -- and it covers exactly one
// label, so it matches a.example.com and neither b.a.example.com nor
// example.com itself.
const WildcardPrefix = "*."

// Route is one host a fleet serves and the ordered backends behind it.
//
// Host is either a name or a wildcard (see WildcardPrefix), which is what an
// operator gets by claiming a custom domain together with its subdomains.
//
// The order is a priority, never a weighting: the first upstream is the one to
// use, and the rest exist for when it fails. With prefer=self the answering
// node's own upstreams come first, so a colocated Caddy that honours the order
// stays on the local process and only leaves the node when it has to.
type Route struct {
	Host        string     `json:"host"`
	Application string     `json:"application"`
	Upstreams   []Upstream `json:"upstreams"`
}

// API describes the fleet API surface, when the answering node publishes it.
// It is absent from the document otherwise.
type API struct {
	// Hosts are the bare platform apexes routed to the daemon itself.
	Hosts []string `json:"hosts"`

	// Upstreams are the daemon's own addresses, ordered like a route's.
	Upstreams []Upstream `json:"upstreams"`

	// Denied are the path prefixes that must never be proxied on those
	// hosts. See Document.DeniesPath.
	Denied []string `json:"denied"`
}

// Document is one fetch of a fleet's routing document.
//
// A Document is immutable once index has run: every lookup reads the maps and
// slices built there and nothing writes to them again, which is what lets one
// value be shared by every request served from that generation.
type Document struct {
	Version int     `json:"v"`
	Routes  []Route `json:"routes"`
	API     *API    `json:"api,omitempty"`

	// Wildcards are the bases the fleet wants a "*.<base>" certificate for:
	// its platform domains and every custom domain claimed together with its
	// subdomains.
	//
	// It is optional on purpose. A node that predates it simply publishes no
	// such member, and then nothing is managed for that fleet and its names
	// are covered on demand exactly as before.
	Wildcards []string `json:"wildcards,omitempty"`

	// hostUpstreams maps a normalized route host onto the upstreams to dial
	// for it, in document order.
	hostUpstreams map[string][]*reverseproxy.Upstream

	// wildcardUpstreams maps the base of a wildcard route -- "example.com" for
	// a host of "*.example.com" -- onto the upstreams to dial for it. It is
	// kept apart from hostUpstreams so that a name is only ever answered by a
	// wildcard once no route names it exactly.
	wildcardUpstreams map[string][]*reverseproxy.Upstream

	// apiHosts is the set of normalized fleet API hosts.
	apiHosts map[string]struct{}

	// apiUpstreams is the daemon's own upstreams, in document order.
	apiUpstreams []*reverseproxy.Upstream

	// deniedPrefixes are the API paths the ingress answers itself, with any
	// trailing separator removed so that matching is uniform.
	deniedPrefixes []string

	// wildcardBases is the normalized, deduplicated Wildcards member.
	wildcardBases []string

	// hash identifies this document's bytes. Two fetches that produced the
	// same bytes produce the same hash, which is how the poller tells a
	// document that changed from one that was merely fetched again. See
	// Routes.Refresh.
	hash uint64
}

// index derives the lookup structures from the decoded document. It is called
// once, on a document nothing else has seen yet, so that a lookup is a map
// lookup and an already-built slice -- Caddy calls it on every request and
// every retry, and asks for it to be instantaneous.
func (d *Document) index() {
	d.hostUpstreams = make(map[string][]*reverseproxy.Upstream, len(d.Routes))
	d.wildcardUpstreams = make(map[string][]*reverseproxy.Upstream)
	for _, route := range d.Routes {
		host := NormalizeHost(route.Host)
		if host == "" {
			continue
		}

		into := d.hostUpstreams
		if strings.Contains(host, "*") {
			base, ok := WildcardBase(host)
			if !ok {
				// A wildcard this module cannot read is dropped, which is
				// what every other entry it cannot use gets: an empty host,
				// a repeated one, an upstream with no address. Routing a
				// name against a pattern only half of the two sides agree
				// on is the one outcome worth refusing outright.
				continue
			}
			host = base
			into = d.wildcardUpstreams
		}

		// The document is sorted by host and holds each one once; if a
		// daemon repeats one anyway, the first wins rather than the last,
		// so the answer does not depend on how far the scan got.
		if _, taken := into[host]; taken {
			continue
		}
		// A route with no usable upstreams is kept, deliberately: the host
		// exists, it simply has nothing running behind it right now. Losing
		// it here would turn that into "no such host", which is a different
		// and wrong answer.
		into[host] = dialTargets(route.Upstreams)
	}

	seenBase := make(map[string]struct{}, len(d.Wildcards))
	for _, base := range d.Wildcards {
		base = NormalizeHost(base)
		// A base is a name, never a pattern: the document says "*.<base>" is
		// wanted by naming the base, so an asterisk here is a shape this build
		// does not read and is dropped like any other unusable entry.
		if base == "" || strings.Contains(base, "*") {
			continue
		}
		if _, taken := seenBase[base]; taken {
			continue
		}
		seenBase[base] = struct{}{}
		d.wildcardBases = append(d.wildcardBases, base)
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

// RouteHosts returns the exact (non-wildcard) route hosts this document names.
func (d *Document) RouteHosts() []string {
	if d == nil {
		return nil
	}
	hosts := make([]string, 0, len(d.hostUpstreams))
	for host := range d.hostUpstreams {
		hosts = append(hosts, host)
	}
	return hosts
}

// WildcardRouteBases returns the bases of the wildcard routes this document
// names: "example.com" for a route host of "*.example.com".
func (d *Document) WildcardRouteBases() []string {
	if d == nil {
		return nil
	}
	bases := make([]string, 0, len(d.wildcardUpstreams))
	for base := range d.wildcardUpstreams {
		bases = append(bases, base)
	}
	return bases
}

// APIHosts returns the fleet API hosts this document names.
func (d *Document) APIHosts() []string {
	if d == nil {
		return nil
	}
	hosts := make([]string, 0, len(d.apiHosts))
	for host := range d.apiHosts {
		hosts = append(hosts, host)
	}
	return hosts
}

// WildcardBases returns the bases the fleet asked for a "*.<base>" certificate
// for, normalized and deduplicated, in document order.
func (d *Document) WildcardBases() []string {
	if d == nil {
		return nil
	}
	return d.wildcardBases
}

// RouteUpstreams returns the upstreams for an exact route host.
func (d *Document) RouteUpstreams(host string) ([]*reverseproxy.Upstream, bool) {
	if d == nil {
		return nil, false
	}
	upstreams, ok := d.hostUpstreams[host]
	return upstreams, ok
}

// WildcardUpstreams returns the upstreams for a wildcard route's base.
func (d *Document) WildcardUpstreams(base string) ([]*reverseproxy.Upstream, bool) {
	if d == nil {
		return nil, false
	}
	upstreams, ok := d.wildcardUpstreams[base]
	return upstreams, ok
}

// APIUpstreams returns the daemon's own upstreams and whether host is one of
// the fleet API hosts.
func (d *Document) APIUpstreams(host string) ([]*reverseproxy.Upstream, bool) {
	if d == nil {
		return nil, false
	}
	if _, ok := d.apiHosts[host]; !ok {
		return nil, false
	}
	return d.apiUpstreams, true
}

// WildcardBase reports the base a document's wildcard host covers, and whether
// the host is a wildcard this module can read at all.
//
// Exactly one leading "*." and no other asterisk anywhere: "*.example.com" is
// the base "example.com", while "*", "*.", "a.*.com", "*.*.com" and "*x.com"
// are none of it. Returning false for those is what keeps a pattern the two
// sides would read differently out of the index.
func WildcardBase(host string) (string, bool) {
	base, found := strings.CutPrefix(host, WildcardPrefix)
	if !found || base == "" || strings.Contains(base, "*") {
		return "", false
	}
	return base, true
}

// WildcardCover reports the base of the one wildcard that could cover host: the
// host with its first label removed, so that a lookup for a.example.com asks
// about "example.com" and nothing else.
//
// A host with no label to give up -- "example", or the empty host -- is covered
// by no wildcard, and neither is a request that carries an asterisk of its own:
// a wildcard covers names, and "*.example.com" asked for literally is not one.
func WildcardCover(host string) (string, bool) {
	label, base, found := strings.Cut(host, ".")
	if !found || label == "" || base == "" || strings.Contains(host, "*") {
		return "", false
	}
	return base, true
}

// DeniesPath reports whether path falls under one of the fleet API's denied
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
func (d *Document) DeniesPath(path string) bool {
	if d == nil || len(d.deniedPrefixes) == 0 {
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

// hashBytes identifies a fetched document by its bytes. It is not a security
// primitive: it exists so a poller can tell "the daemon said the same thing
// again" from "the daemon said something new", and only the latter is allowed
// to rebuild the cross-fleet index or log a conflict.
func hashBytes(body []byte) uint64 {
	h := fnv.New64a()
	_, _ = h.Write(body)
	return h.Sum64()
}
