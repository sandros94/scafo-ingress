// Package permission provides tls.permission.scafo, the on-demand TLS
// permission module: it decides, at handshake time, whether Caddy may obtain a
// certificate for a name.
//
// It answers from the routing documents the scafo app already polls, so a
// handshake costs a map lookup and no HTTP call — which matters, because this
// runs on the handshake path of every name this ingress has no certificate
// for, and an unauthenticated stranger decides how often that is.
package permission

import (
	"context"
	"fmt"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddytls"

	"github.com/sandros94/scafo-ingress/fleets"
)

func init() {
	caddy.RegisterModule(Permission{})
}

// errPrefix is the name this module gives itself in the errors it returns.
const errPrefix = "scafo permission"

// Permission is the Caddy module tls.permission.scafo.
//
// A name is allowed when some fleet's CURRENT routing document names it as an
// exact route host or as a fleet API host. Nothing else is:
//
//   - A name covered only by a `*.` entry is REFUSED. An on-demand certificate
//     per arbitrary subdomain is a door anyone can knock on: point a thousand
//     names at this ingress, and each handshake asks the CA for a certificate
//     under a customer's domain until that customer's weekly issuance limit is
//     gone. The wildcard those names are covered by is obtained once, through
//     DNS-01, and covers all of them for free — so the wildcard is managed and
//     this door stays shut.
//   - A name in no document at all is refused, and so is every name while no
//     document has arrived yet. "I do not know" is not "yes": a cold start
//     must not become an issuance for whatever happens to connect first.
//
// Caddyfile configuration (a global option):
//
//	{
//	  on_demand_tls {
//	    permission scafo
//	  }
//	}
type Permission struct {
	app *fleets.App
}

// interface guards
var (
	_ caddy.Module                = (*Permission)(nil)
	_ caddy.Provisioner           = (*Permission)(nil)
	_ caddytls.OnDemandPermission = (*Permission)(nil)
	_ caddyfile.Unmarshaler       = (*Permission)(nil)
)

// CaddyModule returns the Caddy module information.
func (Permission) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "tls.permission.scafo",
		New: func() caddy.Module { return new(Permission) },
	}
}

// Provision takes a reference to the scafo app.
func (p *Permission) Provision(ctx caddy.Context) error {
	app, err := fleets.FromContext(ctx)
	if err != nil {
		return err
	}
	p.app = app
	return nil
}

// CertificateAllowed reports whether a certificate may be obtained for name.
// A nil error is yes; any error is no, and its text is what Caddy logs.
func (p *Permission) CertificateAllowed(_ context.Context, name string) error {
	if p.app == nil {
		return fmt.Errorf("%s: module has not been provisioned", errPrefix)
	}
	if _, ok := p.app.CertificateAllowed(name); ok {
		return nil
	}
	return fmt.Errorf("%s: no fleet serves %q under a name of its own", errPrefix, name)
}

// UnmarshalCaddyfile parses the module's Caddyfile form, which takes no
// arguments:
//
//	on_demand_tls {
//	    permission scafo
//	}
func (p *Permission) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		if d.NextArg() {
			return d.ArgErr()
		}
		for nesting := d.Nesting(); d.NextBlock(nesting); {
			return d.Errf("unrecognized scafo permission option '%s'", d.Val())
		}
	}
	return nil
}
