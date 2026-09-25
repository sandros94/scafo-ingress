package ingress

import (
	"net/http"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"

	"github.com/sandros94/scafo-ingress/fleets"
)

func init() {
	caddy.RegisterModule(MatchPlainHTTP{})
}

// MatchPlainHTTP is the Caddy request matcher http.matchers.scafo_plain_http.
// It matches a request whose host belongs to a fleet configured with
// `http_redirect off`.
//
// # Why a matcher
//
// HTTP-to-HTTPS redirection is Caddy's own and is on by default, and a fleet
// that turns it off wants ITS hosts served over plain HTTP while every other
// fleet behind the same Caddy keeps the redirect. Caddy decides that per site
// block, from the host names written in the configuration — and this ingress
// has no host names written in its configuration at all: it serves whatever
// the routing documents currently say, and the answer changes without a
// reload. There is nothing for automatic HTTPS to enumerate.
//
// So the decision moves to where Caddy already lets a module decide things per
// request: a matcher. The `http://` site block asks it once, and answers the
// request either by proxying it or by redirecting it:
//
//	http:// {
//	    @plain scafo_plain_http
//	    handle @plain {
//	        reverse_proxy {
//	            dynamic scafo
//	        }
//	    }
//	    handle {
//	        redir https://{host}{uri}
//	    }
//	}
//
// A host no fleet owns does not match, so an unknown name is redirected like
// everything else — the default is the redirect, and only a fleet that asked
// for plain HTTP gets it.
//
// The ACME HTTP challenge is unaffected either way: Caddy answers it in the
// server itself, before any route or matcher runs.
type MatchPlainHTTP struct {
	app *fleets.App
}

// interface guards
var (
	_ caddy.Module                      = (*MatchPlainHTTP)(nil)
	_ caddy.Provisioner                 = (*MatchPlainHTTP)(nil)
	_ caddyhttp.RequestMatcherWithError = (*MatchPlainHTTP)(nil)
	_ caddyfile.Unmarshaler             = (*MatchPlainHTTP)(nil)
)

// CaddyModule returns the Caddy module information.
func (MatchPlainHTTP) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.matchers.scafo_plain_http",
		New: func() caddy.Module { return new(MatchPlainHTTP) },
	}
}

// Provision takes a reference to the scafo app.
func (m *MatchPlainHTTP) Provision(ctx caddy.Context) error {
	app, err := fleets.FromContext(ctx)
	if err != nil {
		return err
	}
	m.app = app
	return nil
}

// MatchWithError reports whether the request's host belongs to a fleet that
// serves it over plain HTTP.
func (m *MatchPlainHTTP) MatchWithError(r *http.Request) (bool, error) {
	if m.app == nil {
		return false, nil
	}
	return m.app.PlainHTTP(r.Host), nil
}

// Match reports whether the request's host belongs to a fleet that serves it
// over plain HTTP. It is kept for the deprecated RequestMatcher interface.
func (m *MatchPlainHTTP) Match(r *http.Request) bool {
	matched, _ := m.MatchWithError(r)
	return matched
}

// UnmarshalCaddyfile parses the matcher's Caddyfile form, which takes no
// arguments:
//
//	@plain scafo_plain_http
func (m *MatchPlainHTTP) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		if d.NextArg() {
			return d.ArgErr()
		}
	}
	return nil
}
