package ingress

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
	"go.uber.org/zap"

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
// Every handler configured alike shares one poller and one document, so the
// cost of the daemon conversation is per configuration and not per site block;
// see routesPool.
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

	// routes is the client and its cached document, shared with every other
	// handler configured the same way. It is read on every request.
	routes *Routes

	// shared is this handler's reference to that poller in routesPool, and is
	// what Cleanup gives back. It is nil when Provision did not get that far.
	shared *sharedRoutes
}

// interface guards
var (
	_ caddy.Module                = (*Module)(nil)
	_ caddy.Provisioner           = (*Module)(nil)
	_ caddy.CleanerUpper          = (*Module)(nil)
	_ reverseproxy.UpstreamSource = (*Module)(nil)
	_ caddyfile.Unmarshaler       = (*Module)(nil)
	_ caddy.Destructor            = (*sharedRoutes)(nil)
)

// routesPool holds the routing-document pollers of the whole process, keyed by
// the conversation each one has with the daemon and reference-counted by the
// handlers using it.
//
// A handler per site block is the normal shape of a Caddyfile -- one snippet
// imported into twenty of them is twenty instances of this module -- and there
// is nothing in that arrangement for twenty pollers to disagree about: they
// would ask the same daemon the same question with the same credential and
// arrive at the same document, twenty times per interval. So they share one.
var routesPool = caddy.NewUsagePool()

// routesKey is the identity of a poller: everything about a handler that
// decides which document comes back, and how often.
//
// The endpoint, the token and the preference decide the answer. The token is
// part of it because a credential is scoped by the daemon that issued it, so
// two handlers holding different ones are not entitled to assume the same
// reply; where the token came from -- inline or a file -- is not, because the
// same secret is the same conversation either way. The interval decides nothing
// about the answer and is still part of the identity: a handler that asks to be
// no more than a second stale must not silently inherit another's five.
//
// The request timeout is absent because it is not configurable. Should it ever
// become so, it belongs here too.
type routesKey struct {
	base     string
	token    string
	prefer   string
	interval time.Duration
}

// sharedRoutes is one poller, the goroutine running it, and the key it is
// pooled under. It lives for as long as at least one handler references it.
type sharedRoutes struct {
	key    routesKey
	routes *Routes

	// stop ends the polling goroutine, and stopped is closed once it has
	// returned.
	//
	// The poller deliberately does not run on the caddy.Context of the handler
	// that provisioned it. That context is cancelled when its configuration is
	// unloaded, which on a reload happens *after* the next configuration's
	// handlers have already taken their own references here -- so honouring it
	// would stop a poller that is still in use. Destruct is the one thing that
	// ends this poller, and the pool only calls it when the last reference is
	// gone.
	stop    context.CancelFunc
	stopped chan struct{}
}

// newSharedRoutes builds a poller, performs its first fetch and starts it.
//
// The first fetch is synchronous and its failure is not returned: it runs while
// the handler that asked for this poller is provisioning, so that a healthy
// pair starts already routing, but a node restarting under a running ingress
// must not be able to keep Caddy from coming back. It is bounded by
// DefaultRequestTimeout like every other fetch.
func newSharedRoutes(key routesKey, endpoint *url.URL, logger *zap.Logger) *sharedRoutes {
	routes := NewRoutes(endpoint, key.token, key.prefer, logger)
	ctx, stop := context.WithCancel(context.Background())
	shared := &sharedRoutes{
		key:     key,
		routes:  routes,
		stop:    stop,
		stopped: make(chan struct{}),
	}

	// Sequenced deliberately: the first fetch happens before the poller exists,
	// so nothing else is ever fetching at the same time.
	routes.Refresh(ctx)
	go func() {
		defer close(shared.stopped)
		routes.Run(ctx, key.interval)
	}()

	return shared
}

// Destruct stops the poller and releases the connections it held. The usage
// pool calls it when the last handler referencing this poller has been cleaned
// up, and never before.
//
// It waits for the polling goroutine to return, so that nothing is still
// talking to the daemon on a departed configuration's behalf once Cleanup has.
// The wait is short: cancelling the context aborts a fetch in flight.
func (s *sharedRoutes) Destruct() error {
	s.stop()
	<-s.stopped
	s.routes.CloseIdleConnections()
	return nil
}

// CaddyModule returns the Caddy module information.
func (Module) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.reverse_proxy.upstreams.scafo",
		New: func() caddy.Module { return new(Module) },
	}
}

// Provision expands placeholders in the configuration, resolves the bearer
// token, and takes a reference to the poller that fetches the routing document
// for this configuration -- starting it if this handler is the first to ask for
// it.
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

	key := routesKey{
		base:     BaseURL(endpoint),
		token:    token,
		prefer:   prefer,
		interval: interval,
	}
	logger := ctx.Logger()

	// One poller per conversation with the daemon, however many handlers want
	// it: the second and the twentieth site block importing the same snippet
	// find this one already running and read the document it has.
	//
	// This is also what carries a reload across without a gap. Caddy provisions
	// the new configuration's handlers before cleaning up the old one's, so a
	// handler whose configuration did not change finds the entry still in the
	// pool and takes a reference to it; the departing handler's Cleanup then
	// drops the count back down. The poller is never stopped and its last good
	// document keeps answering requests throughout -- which means a reload
	// while the daemon is down costs nothing, where a restart would have left
	// the ingress with nothing to route with.
	value, _, err := routesPool.LoadOrNew(key, func() (caddy.Destructor, error) {
		return newSharedRoutes(key, endpoint, logger), nil
	})
	if err != nil {
		return err
	}
	shared := value.(*sharedRoutes)

	// A poller that outlives configurations must not go on writing to the log
	// of one that has been unloaded, so whoever provisions last lends it theirs.
	shared.routes.SetLogger(logger)

	m.shared = shared
	m.routes = shared.routes

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

// Cleanup gives back this handler's reference to the shared poller. The last
// one to go stops it and closes its connections to the daemon, so a shutdown
// leaves neither a goroutine nor a socket behind; on a reload there is no last
// one, because the incoming configuration's handlers took their references
// before this ran.
//
// It is safe to call even when Provision failed or never ran.
func (m *Module) Cleanup() error {
	shared := m.shared
	if shared == nil {
		return nil
	}
	// Dropped before the pool is told, because Delete panics when it is called
	// more often than the key was stored: a second Cleanup must find nothing
	// left to give back. m.routes is deliberately left in place -- a request
	// still in flight on this handler may be reading it, and it answers from
	// the last document either way.
	m.shared = nil
	_, err := routesPool.Delete(shared.key)
	return err
}
