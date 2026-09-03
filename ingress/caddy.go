package ingress

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"

	"github.com/sandros94/scafo-ingress/internal/nodeapi"
)

func init() {
	caddy.RegisterModule(Module{})
}

// Request variables the module sets before every answer, so that an operator
// decides what an unroutable request looks like without this module growing an
// option for it. See Module.GetUpstreams.
const (
	// RouteVar carries an Outcome: ok, denied, unknown or unavailable.
	RouteVar = "scafo.route"

	// HostVar carries the normalized host the lookup was keyed by.
	HostVar = "scafo.host"
)

// Module is the Caddy dynamic upstreams module
// http.reverse_proxy.upstreams.scafo. It answers reverse_proxy's question --
// where does this request go? -- from the fleet routing document a colocated
// scafo node daemon serves.
//
// The routing document is polled in the background and cached, so a request
// costs a map lookup and no daemon round trip. Upstreams come back in the
// document's order, which is a priority and not a weighting: pair it with
// lb_policy first so the head of that order is used and the rest are reached
// for only when it fails.
//
// JSON configuration:
//
//	{
//	  "handler": "reverse_proxy",
//	  "dynamic_upstreams": {
//	    "source": "scafo",
//	    "endpoint": "http://127.0.0.1:7400",
//	    "token": "{env.SCAFO_STORAGE_TOKEN}",
//	    "prefer": "self",
//	    "poll_interval": "5s"
//	  }
//	}
//
// Caddyfile configuration (inside a reverse_proxy block):
//
//	reverse_proxy {
//	  dynamic scafo {
//	    endpoint http://127.0.0.1:7400
//	    token    {env.SCAFO_STORAGE_TOKEN}
//	  }
//	  lb_policy first
//	}
type Module struct {
	// Config carries the endpoint and token options that every scafo module
	// takes. It is embedded, so they remain top-level members of this module's
	// JSON and mean here exactly what they mean in the others.
	nodeapi.Config

	// Prefer names whose upstreams should head every route, and is sent
	// verbatim as the prefer query parameter.
	//
	// "self", the default, means the node that answers the request -- which
	// is what keeps a colocated Caddy proxying to the process next to it.
	// Anything else is read by the daemon as a node id, and one that names no
	// node in the fleet simply has no effect. "none" is this module's own
	// word for asking nothing at all: it sends no parameter, and an absent
	// parameter is how the daemon is told to leave the fleet's own order
	// alone.
	Prefer string `json:"prefer,omitempty"`

	// PollInterval is how long to wait between fetches of the routing
	// document. Default: 5s. It bounds how stale a route may be, which is how
	// long a deployment that has just moved keeps being proxied to where it
	// was.
	PollInterval caddy.Duration `json:"poll_interval,omitempty"`

	// routes is the provisioned client and its cached document.
	routes *Routes
}

// interface guards
var (
	_ caddy.Module                = (*Module)(nil)
	_ caddy.Provisioner           = (*Module)(nil)
	_ caddy.CleanerUpper          = (*Module)(nil)
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

// Provision expands placeholders in the configuration, resolves the bearer
// token, and starts polling the daemon for the routing document.
//
// Only a configuration error fails startup. A daemon that is down at this
// moment does not: the first fetch is attempted here so that a healthy pair
// starts already routing, but its failure is logged and the poller carries on,
// because a node restarting under a running ingress must not be able to keep
// Caddy from coming back.
func (m *Module) Provision(ctx caddy.Context) error {
	endpoint, token, err := m.Config.Resolve(errPrefix)
	if err != nil {
		return err
	}

	// The endpoint and the token are expanded by Resolve; prefer is expanded
	// the same way, so that every value in the block accepts the same
	// placeholders.
	prefer := strings.TrimSpace(caddy.NewReplacer().ReplaceKnown(m.Prefer, ""))
	if prefer == "" {
		prefer = DefaultPrefer
	}

	interval := time.Duration(m.PollInterval)
	switch {
	case interval < 0:
		return fmt.Errorf("%s: poll_interval must be positive, got %s", errPrefix, interval)
	case interval == 0:
		interval = DefaultPollInterval
	}

	m.routes = NewRoutes(endpoint, token, prefer, ctx.Logger())

	// Sequenced deliberately: the synchronous first fetch happens before the
	// poller exists, so nothing else is ever fetching at the same time.
	m.routes.Refresh(ctx)
	go m.routes.Run(ctx, interval)

	return nil
}

// GetUpstreams resolves the backends for a request, implementing
// reverseproxy.UpstreamSource.
//
// The lookup key is the request's Host with any port stripped and lowercased.
// A route wins over a fleet API host of the same name; on an API host, a path
// under one of the daemon's denied prefixes resolves to nothing at all.
//
// A request that resolves to nothing gets an empty slice and no error, which is
// the quiet half of Caddy's two failure modes here. Returning an error would
// make reverse_proxy log one at error level and fall back to its static
// upstreams before reaching the same place; an empty slice goes straight to its
// 503 "no upstreams available" without giving an unknown host a way to fill the
// log.
//
// Which is also why the module writes RouteVar and HostVar before returning:
// 503 is Caddy's word for "nowhere to send this", and an operator who wants an
// unknown host to read as 404 says so in a handle_errors route matching on
// those variables, rather than asking this module for an option that would only
// ever encode one person's preference.
func (m *Module) GetUpstreams(r *http.Request) ([]*reverseproxy.Upstream, error) {
	if m.routes == nil {
		return nil, errors.New("scafo ingress: module has not been provisioned")
	}

	host := NormalizeHost(r.Host)
	upstreams, outcome := m.routes.Document().Lookup(host, r.URL.Path)

	ctx := r.Context()
	caddyhttp.SetVar(ctx, HostVar, host)
	caddyhttp.SetVar(ctx, RouteVar, string(outcome))

	if outcome != OutcomeRoute {
		return nil, nil
	}
	return upstreams, nil
}

// UnmarshalCaddyfile parses the module's Caddyfile block:
//
//	dynamic scafo {
//	    endpoint      <url>
//	    token         <token>
//	    token_file    <path>
//	    prefer        self|<node-id>|none
//	    poll_interval <duration>
//	}
//
// The connection options are handed to the shared parser; the two this module
// adds are its own to read.
//
// Required options, and the fact that token and token_file exclude each other,
// are validated in Provision rather than here, because a JSON configuration
// never passes through this parser.
func (m *Module) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	// The dispenser is positioned before the module name token ("scafo"), so
	// step onto it before reading the block.
	if !d.Next() {
		return d.Err("expected tokens")
	}
	if d.NextArg() {
		// This module takes no inline argument; everything goes in the block.
		return d.ArgErr()
	}
	for d.NextBlock(0) {
		handled, err := m.Config.ParseCaddyfileOption(d)
		if err != nil {
			return err
		}
		if handled {
			continue
		}
		switch d.Val() {
		case "prefer":
			if !d.NextArg() {
				return d.ArgErr()
			}
			m.Prefer = d.Val()
		case "poll_interval":
			if !d.NextArg() {
				return d.ArgErr()
			}
			interval, err := caddy.ParseDuration(d.Val())
			if err != nil {
				return d.Errf("parsing poll_interval: %v", err)
			}
			// Written down and not positive: unlike an omitted value, this
			// cannot be read as "use the default", so say so here rather
			// than quietly substituting one.
			if interval <= 0 {
				return d.Errf("poll_interval must be positive, got %s", interval)
			}
			m.PollInterval = caddy.Duration(interval)
		default:
			return d.Errf("unrecognized scafo ingress option '%s'", d.Val())
		}
		// Each of these options takes exactly one argument, so a second one
		// is a mistake rather than something to ignore.
		if d.NextArg() {
			return d.ArgErr()
		}
	}
	return nil
}

// Cleanup releases the idle connections held by the provisioned client, so a
// configuration reload does not leave sockets to the daemon behind. The poller
// needs nothing here: it runs on the caddy.Context it was provisioned with,
// which Caddy cancels on reload and shutdown. It is safe to call even when
// Provision failed.
func (m *Module) Cleanup() error {
	if m.routes != nil {
		m.routes.CloseIdleConnections()
	}
	return nil
}
