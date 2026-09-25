// Package ingress provides the Caddy HTTP modules that read the scafo app:
// a dynamic upstreams source that answers "where does this request go?", and
// a request matcher that answers "is this host served over plain HTTP?".
//
// Neither holds an endpoint or a token. The fleets, and the credentials that
// open them, are the scafo app's — see the fleets package — so a request is
// resolved across every fleet this Caddy stands in front of at once.
package ingress

import (
	"errors"
	"net/http"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"

	"github.com/sandros94/scafo-ingress/fleets"
)

func init() {
	caddy.RegisterModule(Module{})
}

// errPrefix is the name this module gives itself in the errors it returns.
const errPrefix = "scafo ingress"

// Request variables the module sets before every answer, so that an operator
// decides what an unroutable request looks like without this module growing an
// option for it. See Module.GetUpstreams.
const (
	// RouteVar carries an Outcome: ok, denied, unknown or unavailable.
	RouteVar = "scafo.route"

	// HostVar carries the normalized host the lookup was keyed by, which is
	// always the request's own host and never the pattern that answered it.
	HostVar = "scafo.host"

	// MatchVar carries the document key that answered: a route host, a
	// wildcard pattern such as "*.example.com", or a fleet API host. It is
	// empty when nothing matched, and when there is no document yet.
	MatchVar = "scafo.matched"

	// FleetVar carries the name of the fleet that answered. It is empty when
	// no fleet did, and it is what tells one installation's traffic from
	// another's in an access log.
	FleetVar = "scafo.fleet"
)

// Module is the Caddy dynamic upstreams module
// http.reverse_proxy.upstreams.scafo. It answers reverse_proxy's question --
// where does this request go? -- from the routing documents the scafo app
// polls, across every fleet at once.
//
// The documents are polled in the background and cached, so a request costs a
// map lookup and no daemon round trip. Upstreams come back in the document's
// order, which is a priority and not a weighting: pair it with lb_policy first
// so the head of that order is used and the rest are reached for only when it
// fails.
//
// JSON configuration:
//
//	{
//	  "handler": "reverse_proxy",
//	  "dynamic_upstreams": { "source": "scafo" }
//	}
//
// Caddyfile configuration (inside a reverse_proxy block):
//
//	reverse_proxy {
//	  dynamic scafo
//	  lb_policy first
//	}
type Module struct {
	app *fleets.App
}

// interface guards
var (
	_ caddy.Module                = (*Module)(nil)
	_ caddy.Provisioner           = (*Module)(nil)
	_ reverseproxy.UpstreamSource = (*Module)(nil)
	_ caddyfile.Unmarshaler       = (*Module)(nil)
)

// CaddyModule returns the Caddy module information.
func (Module) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.reverse_proxy.upstreams.scafo",
		New: func() caddy.Module { return new(Module) },
	}
}

// Provision takes a reference to the scafo app.
//
// Only a configuration error fails startup. A daemon that is down at this
// moment does not: each fleet's first fetch is attempted when the app is
// provisioned so that a healthy pair starts already routing, but its failure
// is logged and the poller carries on, because a node restarting under a
// running ingress must not be able to keep Caddy from coming back.
func (m *Module) Provision(ctx caddy.Context) error {
	app, err := fleets.FromContext(ctx)
	if err != nil {
		return err
	}
	m.app = app
	return nil
}

// GetUpstreams resolves the backends for a request, implementing
// reverseproxy.UpstreamSource.
//
// The lookup key is the request's Host with any port stripped and lowercased.
// A route wins over a fleet API host of the same name, and both win over a
// wildcard route covering it; on an API host, a path under one of the daemon's
// denied prefixes resolves to nothing at all. Every fleet is consulted at once
// and the one that published a host first is the one that keeps it. See
// fleets.App.Resolve.
//
// A request that resolves to nothing gets an empty slice and no error, which is
// the quiet half of Caddy's two failure modes here. Returning an error would
// make reverse_proxy log one at error level and fall back to its static
// upstreams before reaching the same place; an empty slice goes straight to its
// 503 "no upstreams available" without giving an unknown host a way to fill the
// log.
//
// Which is also why the module writes its request variables before returning:
// 503 is Caddy's word for "nowhere to send this", and an operator who wants an
// unknown host to read as 404 says so in a handle_errors route matching on
// those variables, rather than asking this module for an option that would only
// ever encode one person's preference.
func (m *Module) GetUpstreams(r *http.Request) ([]*reverseproxy.Upstream, error) {
	if m.app == nil {
		return nil, errors.New(errPrefix + ": module has not been provisioned")
	}

	host := fleets.NormalizeHost(r.Host)
	match := m.app.Resolve(host, r.URL.Path)

	ctx := r.Context()
	caddyhttp.SetVar(ctx, HostVar, host)
	caddyhttp.SetVar(ctx, RouteVar, string(match.Outcome))
	caddyhttp.SetVar(ctx, MatchVar, match.Matched)
	caddyhttp.SetVar(ctx, FleetVar, match.Fleet)

	if match.Outcome != fleets.OutcomeRoute {
		return nil, nil
	}
	return match.Upstreams, nil
}

// UnmarshalCaddyfile parses the module's Caddyfile form, which is the bare
// directive:
//
//	dynamic scafo
//
// It takes no options. Where the fleets are, which token opens them, which
// node's upstreams to prefer and how often to poll are all the scafo global
// option's to say — once, for every module here, so that two site blocks
// cannot disagree about a fleet.
func (m *Module) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	// The dispenser is positioned before the module name token ("scafo"), so
	// step onto it before looking for anything else.
	if !d.Next() {
		return d.Err("expected tokens")
	}
	if d.NextArg() {
		return d.ArgErr()
	}
	for d.NextBlock(0) {
		return d.Errf("unrecognized scafo ingress option '%s'; fleets are configured in the scafo global option", d.Val())
	}
	return nil
}
