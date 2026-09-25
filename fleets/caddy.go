package fleets

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"go.uber.org/zap"
)

func init() {
	// Registered as a pointer: an App carries a mutex, and Caddy only ever
	// needs the value to ask it for its module information.
	caddy.RegisterModule(&App{})
	httpcaddyfile.RegisterGlobalOption("scafo", parseGlobalOption)
}

// AppName is the key the scafo app occupies in Caddy's configuration, and the
// name every other module in this repository asks caddy.Context.App for.
const AppName = "scafo"

// App is the Caddy app module "scafo": the one place a fleet's endpoint and
// token are written down.
//
// Every other module here -- the dynamic upstreams source, the TLS storage,
// the on-demand permission, the plain-HTTP matcher -- reads this app and
// carries no connection settings of its own, so an operator states a fleet
// once and the modules cannot drift apart from each other.
//
// JSON configuration:
//
//	{
//	  "apps": {
//	    "scafo": {
//	      "fleets": [
//	        { "name": "client-a", "endpoint": "http://10.0.0.1:7400", "token": "{env.TOKEN_A}" }
//	      ]
//	    }
//	  }
//	}
//
// Caddyfile configuration (a global option):
//
//	{
//	  scafo {
//	    fleet client-a {
//	      endpoint http://10.0.0.1:7400
//	      token    {env.TOKEN_A}
//	    }
//	  }
//	}
//
// An app that declares NO fleet reads them from the process environment
// instead; see env.go. That is what lets the published image run with nothing
// but environment variables and the Caddyfile it already ships.
type App struct {
	// FleetsRaw are the fleets this Caddy serves. When it is empty the
	// environment is read instead -- never both, so the two can never be
	// half-merged.
	FleetsRaw []Fleet `json:"fleets,omitempty"`

	// DNS are the DNS-01 providers used for the wildcard certificates a
	// fleet asks for. With none configured nothing is managed and every
	// certificate is obtained on demand for an exact name.
	DNS []DNSProvider `json:"dns,omitempty"`

	// ACMEDelegation is the fixed name the DNS challenge record is written
	// at for a base under none of the operator's own zones. The customer
	// creates `_acme-challenge.<base> CNAME <this>`, and the challenge is
	// solved in the operator's zone rather than theirs.
	ACMEDelegation string `json:"acme_delegation,omitempty"`

	// ACMEEmail is the contact address registered with the ACME CA.
	ACMEEmail string `json:"acme_email,omitempty"`

	// ACMECA is the ACME directory URL to use for the managed wildcards.
	// Leaving it unset uses Caddy's default. It exists so a test can point
	// at a local CA; never point a test at a production CA.
	ACMECA string `json:"acme_ca,omitempty"`

	ctx    caddy.Context
	logger *zap.Logger

	fleets []*Fleet
	byName map[string]*Fleet

	// mu guards claims, conflicts and the index rebuild.
	mu        sync.Mutex
	claims    map[claimKey]string
	conflicts map[claimKey]string
	index     atomic.Pointer[index]

	wild *wildcardManager
}

// interface guards
var (
	_ caddy.Module      = (*App)(nil)
	_ caddy.Provisioner = (*App)(nil)
	_ caddy.App         = (*App)(nil)
)

// CaddyModule returns the Caddy module information.
func (*App) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  AppName,
		New: func() caddy.Module { return new(App) },
	}
}

// FromContext returns the scafo app, instantiating it if the configuration
// does not declare one -- which is the env-only case, where the app's whole
// configuration is in the environment and there is nothing to write down.
func FromContext(ctx caddy.Context) (*App, error) {
	value, err := ctx.App(AppName)
	if err != nil {
		return nil, err
	}
	app, ok := value.(*App)
	if !ok {
		return nil, fmt.Errorf("%s: the %s app is %T, not this module's", errPrefix, AppName, value)
	}
	return app, nil
}

// Provision resolves every fleet, starts its poller, and builds the DNS
// provider configuration.
//
// It deliberately does not touch the TLS app: this app is provisioned from
// inside the storage module, which Caddy sets up before any app, so reaching
// for the TLS app here would ask Caddy to provision it in the middle of
// provisioning its own storage. Everything that needs the TLS app happens in
// Start.
func (a *App) Provision(ctx caddy.Context) error {
	a.ctx = ctx
	a.logger = ctx.Logger()
	a.claims = map[claimKey]string{}
	a.conflicts = map[claimKey]string{}
	a.byName = map[string]*Fleet{}

	declared := a.FleetsRaw
	fromEnv := false
	if len(declared) == 0 {
		env, err := fleetsFromEnv(osEnvironment())
		if err != nil {
			return err
		}
		declared = env
		fromEnv = true
	}
	if len(declared) == 0 {
		return fmt.Errorf("%s: no fleet is configured; declare one in the scafo global option or set %sENDPOINT and %sTOKEN",
			errPrefix, fleetPrefix, fleetPrefix)
	}

	for i := range declared {
		fleet := &declared[i]
		if err := fleet.resolve(); err != nil {
			return err
		}
		if _, taken := a.byName[fleet.Name]; taken {
			return fmt.Errorf("%s: fleet %q is declared twice", errPrefix, fleet.Name)
		}
		a.byName[fleet.Name] = fleet
		a.fleets = append(a.fleets, fleet)
	}

	if len(a.DNS) == 0 {
		providers, err := dnsProvidersFromEnv(osEnvironment())
		if err != nil {
			return err
		}
		a.DNS = providers
	}
	seenProvider := map[string]struct{}{}
	for i := range a.DNS {
		provider := &a.DNS[i]
		if err := provider.resolve(); err != nil {
			return err
		}
		if _, taken := seenProvider[provider.Name]; taken {
			return fmt.Errorf("%s: DNS provider %q is declared twice", errPrefix, provider.describe())
		}
		seenProvider[provider.Name] = struct{}{}
	}

	repl := caddy.NewReplacer()
	a.ACMEDelegation = NormalizeHost(repl.ReplaceKnown(a.ACMEDelegation, ""))
	a.ACMEEmail = strings.TrimSpace(repl.ReplaceKnown(a.ACMEEmail, ""))
	a.ACMECA = strings.TrimSpace(repl.ReplaceKnown(a.ACMECA, ""))
	if a.ACMEDelegation == "" {
		a.ACMEDelegation = NormalizeHost(osEnvironment()[acmeDelegationVar])
	}
	if a.ACMEEmail == "" {
		a.ACMEEmail = strings.TrimSpace(osEnvironment()[acmeEmailVar])
	}
	if a.ACMECA == "" {
		a.ACMECA = strings.TrimSpace(osEnvironment()[acmeCAVar])
	}

	// One poller per conversation with a daemon, shared across reloads: the
	// incoming configuration takes its reference before the outgoing one
	// gives its own back, so polling never pauses and the last document that
	// parsed keeps answering requests throughout.
	for _, fleet := range a.fleets {
		key := routesKey{
			base:     BaseURL(fleet.endpoint),
			token:    fleet.token,
			prefer:   fleet.Prefer,
			interval: time.Duration(fleet.PollInterval),
			timeout:  time.Duration(fleet.Timeout),
		}
		logger := a.logger.With(zap.String("fleet", fleet.Name))
		value, _, err := routesPool.LoadOrNew(key, func() (caddy.Destructor, error) {
			return newSharedRoutes(key, fleet, logger), nil
		})
		if err != nil {
			return err
		}
		shared := value.(*sharedRoutes)
		shared.routes.SetLogger(logger)
		shared.routes.OnChange(a.invalidate)
		fleet.shared = shared
		fleet.routes = shared.routes
	}

	if fromEnv {
		a.logger.Info("fleets configured from the environment",
			zap.Strings("fleets", a.fleetNames()))
	}
	return nil
}

// fleetNames lists the app's fleets, for a log line.
func (a *App) fleetNames() []string {
	names := make([]string, 0, len(a.fleets))
	for _, fleet := range a.fleets {
		names = append(names, fleet.Name)
	}
	return names
}

// Start begins managing the wildcard certificates the fleets ask for. It runs
// after every app has been provisioned, which is when the TLS app exists to
// be asked.
func (a *App) Start() error {
	a.wild = newWildcardManager(a)
	return a.wild.start()
}

// Stop ends the wildcard manager and gives back this app's references to the
// shared pollers. The last reference to a poller stops it and closes its
// connections; on a reload there is no last one, because the incoming app took
// its references first.
func (a *App) Stop() error {
	if a.wild != nil {
		a.wild.stop()
		a.wild = nil
	}
	var firstErr error
	for _, fleet := range a.fleets {
		if fleet.shared == nil {
			continue
		}
		shared := fleet.shared
		fleet.shared = nil
		if _, err := routesPool.Delete(shared.key); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// routesPool holds the routing-document pollers of the whole process, keyed by
// the conversation each one has with a daemon and reference-counted by the
// configurations using it. It is what carries a reload across without a gap.
var routesPool = caddy.NewUsagePool()

// routesKey is the identity of a poller: everything about a fleet that decides
// which document comes back, and how often.
//
// The token is part of it because a credential is scoped by the daemon that
// issued it, so two fleets holding different ones are not entitled to assume
// the same reply; where the token came from -- inline or a file -- is not,
// because the same secret is the same conversation either way.
type routesKey struct {
	base     string
	token    string
	prefer   string
	interval time.Duration
	timeout  time.Duration
}

// sharedRoutes is one poller, the goroutine running it, and the key it is
// pooled under. It lives for as long as at least one configuration references
// it.
type sharedRoutes struct {
	key    routesKey
	routes *Routes

	// stop ends the polling goroutine, and stopped is closed once it has
	// returned.
	//
	// The poller deliberately does not run on the caddy.Context of the app
	// that provisioned it. That context is cancelled when its configuration is
	// unloaded, which on a reload happens *after* the next configuration has
	// already taken its own reference here -- so honouring it would stop a
	// poller that is still in use. Destruct is the one thing that ends this
	// poller, and the pool only calls it when the last reference is gone.
	stop    context.CancelFunc
	stopped chan struct{}
}

// newSharedRoutes builds a poller, performs its first fetch and starts it.
//
// The first fetch is synchronous and its failure is not returned: it runs while
// the app that asked for this poller is provisioning, so that a healthy pair
// starts already routing, but a node restarting under a running ingress must
// not be able to keep Caddy from coming back.
func newSharedRoutes(key routesKey, fleet *Fleet, logger *zap.Logger) *sharedRoutes {
	routes := NewRoutes(fleet.endpoint, key.token, key.prefer, key.timeout, logger)
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
// pool calls it when the last configuration referencing this poller has been
// stopped, and never before.
func (s *sharedRoutes) Destruct() error {
	s.stop()
	<-s.stopped
	s.routes.CloseIdleConnections()
	return nil
}

var _ caddy.Destructor = (*sharedRoutes)(nil)

// parseGlobalOption parses the scafo global option:
//
//	scafo {
//	    fleet <name> {
//	        endpoint        <url>
//	        token           <token>
//	        token_file      <path>
//	        prefer          self|<node-id>|none
//	        poll_interval   <duration>
//	        timeout         <duration>
//	        storage_timeout <duration>
//	        http_redirect   on|off
//	    }
//	    dns <module> {
//	        <field> <value>
//	    }
//	    dns <name> <module> {
//	        zones   <zone...>
//	        <field> <value>
//	    }
//	    acme_delegation <fqdn>
//	    acme_email      <email>
//	    acme_ca         <url>
//	}
//
// An empty block is meaningful and is what the image's default Caddyfile
// carries: it declares the app so that the environment can configure it.
func parseGlobalOption(d *caddyfile.Dispenser, _ any) (any, error) {
	d.Next() // consume the option name
	app := new(App)
	if d.NextArg() {
		return nil, d.ArgErr()
	}
	for d.NextBlock(0) {
		switch d.Val() {
		case "fleet":
			fleet, err := parseFleet(d)
			if err != nil {
				return nil, err
			}
			app.FleetsRaw = append(app.FleetsRaw, fleet)
		case "dns":
			provider, err := parseDNSProvider(d)
			if err != nil {
				return nil, err
			}
			app.DNS = append(app.DNS, provider)
		case "acme_delegation":
			if !d.NextArg() {
				return nil, d.ArgErr()
			}
			app.ACMEDelegation = d.Val()
			if d.NextArg() {
				return nil, d.ArgErr()
			}
		case "acme_email":
			if !d.NextArg() {
				return nil, d.ArgErr()
			}
			app.ACMEEmail = d.Val()
			if d.NextArg() {
				return nil, d.ArgErr()
			}
		case "acme_ca":
			if !d.NextArg() {
				return nil, d.ArgErr()
			}
			app.ACMECA = d.Val()
			if d.NextArg() {
				return nil, d.ArgErr()
			}
		default:
			return nil, d.Errf("unrecognized scafo option '%s'", d.Val())
		}
	}

	return httpcaddyfile.App{
		Name:  AppName,
		Value: caddyconfig.JSON(app, nil),
	}, nil
}

// parseFleet reads one `fleet <name> { … }` block. The dispenser is positioned
// on the "fleet" token.
func parseFleet(d *caddyfile.Dispenser) (Fleet, error) {
	var fleet Fleet
	if !d.NextArg() {
		return fleet, d.Err("a fleet must be named")
	}
	fleet.Name = d.Val()
	if d.NextArg() {
		return fleet, d.ArgErr()
	}
	for nesting := d.Nesting(); d.NextBlock(nesting); {
		option := d.Val()
		handled, err := fleet.Config.ParseCaddyfileOption(d)
		if err != nil {
			return fleet, err
		}
		if handled {
			continue
		}
		switch option {
		case "prefer":
			if !d.NextArg() {
				return fleet, d.ArgErr()
			}
			fleet.Prefer = d.Val()
		case "poll_interval", "timeout", "storage_timeout":
			if !d.NextArg() {
				return fleet, d.ArgErr()
			}
			duration, err := caddy.ParseDuration(d.Val())
			if err != nil {
				return fleet, d.Errf("parsing %s: %v", option, err)
			}
			// Written down and not positive: unlike an omitted value, this
			// cannot be read as "use the default", so say so here rather than
			// quietly substituting one.
			if duration <= 0 {
				return fleet, d.Errf("%s must be positive, got %s", option, duration)
			}
			switch option {
			case "poll_interval":
				fleet.PollInterval = caddy.Duration(duration)
			case "timeout":
				fleet.Timeout = caddy.Duration(duration)
			default:
				fleet.StorageTimeout = caddy.Duration(duration)
			}
		case "http_redirect":
			if !d.NextArg() {
				return fleet, d.ArgErr()
			}
			on, err := parseBool(d.Val())
			if err != nil {
				return fleet, d.Errf("http_redirect: %v", err)
			}
			fleet.HTTPRedirect = &on
		default:
			return fleet, d.Errf("unrecognized scafo fleet option '%s'", option)
		}
		if d.NextArg() {
			return fleet, d.ArgErr()
		}
	}
	return fleet, nil
}

// parseDNSProvider reads one `dns [<name>] <module> { … }` block. Every option
// in the block is one of the provider module's own JSON fields, except zones,
// which is this app's. The Caddyfile form therefore mirrors the environment
// form exactly, and an operator who has read one has read the other.
func parseDNSProvider(d *caddyfile.Dispenser) (DNSProvider, error) {
	var provider DNSProvider
	args := d.RemainingArgs()
	switch len(args) {
	case 1:
		provider.Module = args[0]
	case 2:
		provider.Name = args[0]
		provider.Module = args[1]
	default:
		return provider, d.Err("dns takes either a module name, or a name and a module name")
	}
	provider.Fields = map[string]string{}
	for nesting := d.Nesting(); d.NextBlock(nesting); {
		field := d.Val()
		if field == "zones" {
			zones := d.RemainingArgs()
			if len(zones) == 0 {
				return provider, d.ArgErr()
			}
			provider.Zones = append(provider.Zones, zones...)
			continue
		}
		if !d.NextArg() {
			return provider, d.ArgErr()
		}
		if _, taken := provider.Fields[field]; taken {
			return provider, d.Errf("dns field '%s' is set twice", field)
		}
		provider.Fields[field] = d.Val()
		if d.NextArg() {
			return provider, d.ArgErr()
		}
	}
	return provider, nil
}
