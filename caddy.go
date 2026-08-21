package scafo

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/certmagic"
)

func init() {
	caddy.RegisterModule(Module{})
}

// Module is the Caddy storage module caddy.storage.scafo. It configures a
// Storage client and hands it to Caddy as the TLS asset store.
//
// Caddy never learns the credentials of the underlying bucket: it is given a
// loopback endpoint and a bearer token file, both provisioned by the scafo
// agent that owns the bucket.
//
// JSON configuration:
//
//	{
//	  "storage": {
//	    "module": "scafo",
//	    "endpoint": "http://127.0.0.1:7400",
//	    "token_file": "/var/lib/scafo/storage-token"
//	  }
//	}
//
// Caddyfile configuration (a global option):
//
//	{
//	  storage scafo {
//	    endpoint   http://127.0.0.1:7400
//	    token_file /var/lib/scafo/storage-token
//	  }
//	}
type Module struct {
	// Endpoint is the base URL of the scafo agent's storage API, for example
	// http://127.0.0.1:7400. A path prefix is allowed and is preserved.
	// Required.
	Endpoint string `json:"endpoint,omitempty"`

	// TokenFile is the path to a file containing the bearer token to present
	// to the agent. The file is read once during provisioning and its content
	// is trimmed of surrounding whitespace. Required.
	TokenFile string `json:"token_file,omitempty"`

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
// and reads the bearer token.
//
// The token is read here, once, rather than per request: storage is provisioned
// before Caddy serves anything, so an unreadable or empty token file must fail
// startup loudly instead of surfacing later as certificate errors.
func (m *Module) Provision(ctx caddy.Context) error {
	// Storage is provisioned before the apps that own the request replacer, so
	// only global placeholders (env vars, system values) are available here.
	// ReplaceKnown, rather than ReplaceAll, leaves unrecognized braces alone
	// instead of silently blanking part of a path or URL.
	repl := caddy.NewReplacer()

	endpoint := strings.TrimSpace(repl.ReplaceKnown(m.Endpoint, ""))
	if endpoint == "" {
		return errors.New("scafo storage: endpoint is required")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("scafo storage: invalid endpoint %q: %v", endpoint, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("scafo storage: endpoint %q must use the http or https scheme", endpoint)
	}
	if parsed.Host == "" {
		return fmt.Errorf("scafo storage: endpoint %q has no host", endpoint)
	}
	// Only the origin and any path prefix are meaningful; the rest would be
	// silently dropped when protocol paths are appended.
	parsed.RawQuery = ""
	parsed.Fragment = ""
	parsed.User = nil

	tokenFile := strings.TrimSpace(repl.ReplaceKnown(m.TokenFile, ""))
	if tokenFile == "" {
		return errors.New("scafo storage: token_file is required")
	}
	raw, err := os.ReadFile(tokenFile)
	if err != nil {
		return fmt.Errorf("scafo storage: reading token_file: %v", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return fmt.Errorf("scafo storage: token_file %s is empty", tokenFile)
	}

	m.storage = NewStorage(parsed, token)
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
//	    token_file <path>
//	}
//
// Required options are validated in Provision rather than here, because a JSON
// configuration never passes through this parser.
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
		switch d.Val() {
		case "endpoint":
			if !d.NextArg() {
				return d.ArgErr()
			}
			m.Endpoint = d.Val()
		case "token_file":
			if !d.NextArg() {
				return d.ArgErr()
			}
			m.TokenFile = d.Val()
		default:
			return d.Errf("unrecognized scafo storage option '%s'", d.Val())
		}
		if d.NextArg() {
			return d.ArgErr()
		}
	}
	return nil
}

// Cleanup releases the idle connections held by the provisioned client, so a
// configuration reload does not leave sockets to the agent behind. It is safe
// to call even when Provision failed.
func (m *Module) Cleanup() error {
	if m.storage != nil {
		m.storage.CloseIdleConnections()
	}
	return nil
}
