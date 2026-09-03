// Package nodeapi holds what every Caddy module in this repository needs in
// order to talk to a colocated scafo node daemon: the endpoint and token
// options an operator configures, and the error handling for the daemon's
// problem+json responses.
//
// The daemon owns the credentials of whatever it fronts -- a bucket, the node's
// own state -- and hands out nothing but a loopback endpoint URL and a bearer
// token. Every module here therefore configures the same two values and reads
// the same error documents back, which is exactly what this package holds. What
// each module does with the connection -- the paths it calls, the headers it
// adds, the protocol version it speaks -- stays in that module.
package nodeapi

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

// Config is the connection to the scafo node daemon as an operator states it.
// Modules embed it so that its fields, and their JSON names, are the same
// everywhere.
type Config struct {
	// Endpoint is the base URL of the scafo node daemon's API, for example
	// http://127.0.0.1:7400. A path prefix is allowed and is preserved.
	// Required.
	Endpoint string `json:"endpoint,omitempty"`

	// Token is the bearer token to present to the daemon, given inline. It is
	// trimmed of surrounding whitespace, exactly as file content is. Exactly
	// one of Token and TokenFile is required.
	Token string `json:"token,omitempty"`

	// TokenFile is the path to a file containing the bearer token to present
	// to the daemon. The file is read once during provisioning and its content
	// is trimmed of surrounding whitespace. Exactly one of Token and TokenFile
	// is required.
	TokenFile string `json:"token_file,omitempty"`
}

// Resolve expands placeholders in the configuration, validates the endpoint and
// resolves the bearer token, returning both.
//
// prefix names the calling module in the errors returned here, without
// punctuation ("scafo storage"); the separator is added. Callers resolve during
// provisioning rather than per request: a module is provisioned before Caddy
// serves anything, so a missing, unreadable or empty token must fail startup
// loudly instead of surfacing later as request failures.
func (c Config) Resolve(prefix string) (*url.URL, string, error) {
	// Modules are provisioned before the apps that own the request replacer, so
	// only global placeholders (env vars, system values) are available here.
	// ReplaceKnown, rather than ReplaceAll, leaves unrecognized braces alone
	// instead of silently blanking part of a path or URL.
	repl := caddy.NewReplacer()

	endpoint := strings.TrimSpace(repl.ReplaceKnown(c.Endpoint, ""))
	if endpoint == "" {
		return nil, "", fmt.Errorf("%s: endpoint is required", prefix)
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, "", fmt.Errorf("%s: invalid endpoint %q: %v", prefix, endpoint, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, "", fmt.Errorf("%s: endpoint %q must use the http or https scheme", prefix, endpoint)
	}
	if parsed.Host == "" {
		return nil, "", fmt.Errorf("%s: endpoint %q has no host", prefix, endpoint)
	}
	// Only the origin and any path prefix are meaningful; the rest would be
	// silently dropped when protocol paths are appended.
	parsed.RawQuery = ""
	parsed.Fragment = ""
	parsed.User = nil

	// The two token sources converge on one string here, so nothing downstream
	// can tell -- or needs to tell -- where the credential came from. They are
	// mutually exclusive rather than ordered by precedence: a deployment that
	// sets both has a mistake in it, and silently preferring one would hide
	// which token is actually in use.
	token := strings.TrimSpace(repl.ReplaceKnown(c.Token, ""))
	tokenFile := strings.TrimSpace(repl.ReplaceKnown(c.TokenFile, ""))
	switch {
	case token != "" && tokenFile != "":
		return nil, "", fmt.Errorf("%s: token and token_file are mutually exclusive; set exactly one", prefix)
	case token == "" && tokenFile == "":
		return nil, "", fmt.Errorf("%s: either token or token_file is required", prefix)
	case tokenFile != "":
		raw, err := os.ReadFile(tokenFile)
		if err != nil {
			return nil, "", fmt.Errorf("%s: reading token_file: %v", prefix, err)
		}
		token = strings.TrimSpace(string(raw))
		if token == "" {
			return nil, "", fmt.Errorf("%s: token_file %s is empty", prefix, tokenFile)
		}
	}

	return parsed, token, nil
}

// ParseCaddyfileOption reads the option the dispenser is positioned on when it
// is one of endpoint, token or token_file, and reports whether it was:
//
//	endpoint   <url>
//	token      <token>
//	token_file <path>
//
// A module's own options are its own to parse, so an option this package does
// not know is left untouched and reported as unhandled, letting the caller
// either recognize it or reject it in its own words.
//
// Required options, and the fact that token and token_file exclude each other,
// are validated by Resolve rather than here, because a JSON configuration never
// passes through this parser.
func (c *Config) ParseCaddyfileOption(d *caddyfile.Dispenser) (bool, error) {
	switch d.Val() {
	case "endpoint":
		if !d.NextArg() {
			return true, d.ArgErr()
		}
		c.Endpoint = d.Val()
	case "token":
		if !d.NextArg() {
			return true, d.ArgErr()
		}
		c.Token = d.Val()
	case "token_file":
		if !d.NextArg() {
			return true, d.ArgErr()
		}
		c.TokenFile = d.Val()
	default:
		return false, nil
	}
	// Each of these options takes exactly one argument, so a second one is a
	// mistake rather than something to ignore.
	if d.NextArg() {
		return true, d.ArgErr()
	}
	return true, nil
}
