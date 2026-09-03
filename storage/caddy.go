package storage

import (
	"errors"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/certmagic"

	"github.com/sandros94/scafo-ingress/internal/nodeapi"
)

func init() {
	caddy.RegisterModule(Module{})
}

// Module is the Caddy storage module caddy.storage.scafo. It configures a
// Storage client and hands it to Caddy as the TLS asset store.
//
// Caddy never learns the credentials of the underlying bucket: it is given a
// loopback endpoint and a bearer token, both provisioned by the scafo node
// daemon that owns the bucket.
//
// JSON configuration:
//
//	{
//	  "storage": {
//	    "module": "scafo",
//	    "endpoint": "http://127.0.0.1:7400",
//	    "token": "{env.SCAFO_STORAGE_TOKEN}"
//	  }
//	}
//
// Caddyfile configuration (a global option):
//
//	{
//	  storage scafo {
//	    endpoint http://127.0.0.1:7400
//	    token    {env.SCAFO_STORAGE_TOKEN}
//	  }
//	}
//
// The token may instead be read from a file, which is the better fit wherever
// a credential manager hands one over as a path:
//
//	{
//	  storage scafo {
//	    endpoint   http://127.0.0.1:7400
//	    token_file /var/lib/scafo/storage-token
//	  }
//	}
type Module struct {
	// Config carries the endpoint and token options that every scafo module
	// takes. It is embedded, so they remain top-level members of this module's
	// JSON and mean here exactly what they mean in the others.
	nodeapi.Config

	// storage is the provisioned client handed to certmagic.
	storage *Storage
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

// Provision expands placeholders in the configuration, validates the endpoint
// and resolves the bearer token.
//
// The token is resolved here, once, rather than per request: storage is
// provisioned before Caddy serves anything, so a missing, unreadable or empty
// token must fail startup loudly instead of surfacing later as certificate
// errors.
func (m *Module) Provision(ctx caddy.Context) error {
	endpoint, token, err := m.Config.Resolve(errPrefix)
	if err != nil {
		return err
	}
	m.storage = NewStorage(endpoint, token)
	return nil
}

// CertMagicStorage returns the provisioned storage client.
func (m *Module) CertMagicStorage() (certmagic.Storage, error) {
	if m.storage == nil {
		return nil, errors.New("scafo storage: module has not been provisioned")
	}
	return m.storage, nil
}

// UnmarshalCaddyfile parses the module's Caddyfile block:
//
//	storage scafo {
//	    endpoint   <url>
//	    token      <token>
//	    token_file <path>
//	}
//
// Every option in the block is a connection option, so each one is handed to
// the shared parser; this module adds none of its own.
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
		if !handled {
			return d.Errf("unrecognized scafo storage option '%s'", d.Val())
		}
	}
	return nil
}

// Cleanup releases the idle connections held by the provisioned client, so a
// configuration reload does not leave sockets to the daemon behind. It is safe
// to call even when Provision failed.
func (m *Module) Cleanup() error {
	if m.storage != nil {
		m.storage.CloseIdleConnections()
	}
	return nil
}
