package storage

import (
	"errors"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/certmagic"

	"github.com/sandros94/scafo-ingress/fleets"
)

func init() {
	caddy.RegisterModule(Module{})
}

// Module is the Caddy storage module caddy.storage.scafo. It hands Caddy a
// store that routes every key to the fleet that owns the domain it names.
//
// Caddy never learns the credentials of any fleet's bucket: each node daemon
// owns its own, and this module holds nothing but the endpoints and bearer
// tokens the scafo app was configured with.
//
// It takes no options. The fleets it uses are the scafo app's, so a token is
// written down once for the whole ingress rather than once per module:
//
//	{
//	  scafo {
//	    fleet client-a {
//	      endpoint http://10.0.0.1:7400
//	      token    {env.TOKEN_A}
//	    }
//	  }
//	  storage scafo
//	}
//
// JSON configuration:
//
//	{ "storage": { "module": "scafo" } }
type Module struct {
	router *Router
}

// interface guards
var (
	_ caddy.Module           = (*Module)(nil)
	_ caddy.Provisioner      = (*Module)(nil)
	_ caddy.CleanerUpper     = (*Module)(nil)
	_ caddy.StorageConverter = (*Module)(nil)
	_ caddyfile.Unmarshaler  = (*Module)(nil)
)

// CaddyModule returns the Caddy module information.
func (Module) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "caddy.storage.scafo",
		New: func() caddy.Module { return new(Module) },
	}
}

// Provision takes the scafo app's fleets and builds a client for each.
//
// Caddy provisions storage before any app, so this is normally what brings the
// scafo app into being — which is why that app's own Provision reaches for
// nothing but the environment and its own configuration. A missing, unreadable
// or empty token fails startup here, loudly, rather than surfacing later as
// certificate errors.
func (m *Module) Provision(ctx caddy.Context) error {
	app, err := fleets.FromContext(ctx)
	if err != nil {
		return err
	}
	// Keys that name no domain have no fleet to belong to, so they stay on
	// this machine: Caddy's own file storage, under its data directory, which
	// is exactly where they would live with no storage module configured at
	// all. The ACME account is the important one — it is the ingress's, not
	// any fleet's, and putting it in one fleet's bucket would make that fleet
	// able to impersonate the ingress to the CA.
	return m.provisionWith(app, caddy.DefaultStorage)
}

// provisionWith builds the router from an app and a local store. It is
// separate so a test can supply a local store of its own.
func (m *Module) provisionWith(app *fleets.App, local certmagic.Storage) error {
	m.router = NewRouter(app, local)
	return nil
}

// CertMagicStorage returns the provisioned router.
func (m *Module) CertMagicStorage() (certmagic.Storage, error) {
	if m.router == nil {
		return nil, errors.New("scafo storage: module has not been provisioned")
	}
	return m.router, nil
}

// UnmarshalCaddyfile parses the module's Caddyfile form, which is the bare
// directive:
//
//	storage scafo
//
// It takes no options at all. Where the fleets are, and which token opens
// them, is the scafo global option's to say — once, for every module here.
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
		return d.Errf("unrecognized scafo storage option '%s'; fleets are configured in the scafo global option", d.Val())
	}
	return nil
}

// Cleanup releases the idle connections held by the provisioned clients, so a
// configuration reload does not leave sockets to the daemons behind. It is
// safe to call even when Provision failed.
func (m *Module) Cleanup() error {
	if m.router != nil {
		m.router.CloseIdleConnections()
	}
	return nil
}
