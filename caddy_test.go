package scafo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

// writeTokenFile creates a token file in a temporary directory and returns its
// path.
func writeTokenFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "storage-token")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing token file: %v", err)
	}
	return path
}

func TestProvisionReadsAndTrimsToken(t *testing.T) {
	// Agents commonly write the token with a trailing newline.
	tokenFile := writeTokenFile(t, "  token-from-agent\n")
	module := &Module{
		Endpoint:  "http://127.0.0.1:7400",
		TokenFile: tokenFile,
	}

	if err := module.Provision(caddy.Context{}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	storage, err := module.CertMagicStorage()
	if err != nil {
		t.Fatalf("CertMagicStorage: %v", err)
	}
	client, ok := storage.(*Storage)
	if !ok {
		t.Fatalf("CertMagicStorage returned %T, want *Storage", storage)
	}
	if client.token != "token-from-agent" {
		t.Errorf("token = %q, want %q", client.token, "token-from-agent")
	}
	if client.base != "http://127.0.0.1:7400" {
		t.Errorf("base = %q, want %q", client.base, "http://127.0.0.1:7400")
	}
}

func TestProvisionTrimsTrailingSlashFromEndpoint(t *testing.T) {
	module := &Module{
		Endpoint:  "http://127.0.0.1:7400/",
		TokenFile: writeTokenFile(t, "tok"),
	}
	if err := module.Provision(caddy.Context{}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if module.storage.base != "http://127.0.0.1:7400" {
		t.Errorf("base = %q, want %q", module.storage.base, "http://127.0.0.1:7400")
	}
}

func TestProvisionErrors(t *testing.T) {
	existing := writeTokenFile(t, "tok")
	missing := filepath.Join(t.TempDir(), "nope", "storage-token")

	tests := []struct {
		name    string
		module  Module
		wantErr string
	}{
		{
			name:    "missing token file",
			module:  Module{Endpoint: "http://127.0.0.1:7400", TokenFile: missing},
			wantErr: "reading token_file",
		},
		{
			name:    "empty token file",
			module:  Module{Endpoint: "http://127.0.0.1:7400", TokenFile: writeTokenFile(t, "\n \n")},
			wantErr: "is empty",
		},
		{
			name:    "no token file configured",
			module:  Module{Endpoint: "http://127.0.0.1:7400"},
			wantErr: "token_file is required",
		},
		{
			name:    "no endpoint configured",
			module:  Module{TokenFile: existing},
			wantErr: "endpoint is required",
		},
		{
			name:    "unparseable endpoint",
			module:  Module{Endpoint: "127.0.0.1:7400", TokenFile: existing},
			wantErr: "invalid endpoint",
		},
		{
			name:    "endpoint with a non-http scheme",
			module:  Module{Endpoint: "unix:///run/scafo.sock", TokenFile: existing},
			wantErr: "must use the http or https scheme",
		},
		{
			name:    "endpoint without host",
			module:  Module{Endpoint: "http:///v1", TokenFile: existing},
			wantErr: "has no host",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			module := tt.module
			err := module.Provision(caddy.Context{})
			if err == nil {
				t.Fatalf("Provision error = nil, want an error mentioning %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Provision error = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestProvisionExpandsPlaceholders(t *testing.T) {
	tokenFile := writeTokenFile(t, "tok")
	t.Setenv("SCAFO_TEST_ENDPOINT", "http://127.0.0.1:9999")
	t.Setenv("SCAFO_TEST_TOKEN_FILE", tokenFile)

	module := &Module{
		Endpoint:  "{env.SCAFO_TEST_ENDPOINT}",
		TokenFile: "{env.SCAFO_TEST_TOKEN_FILE}",
	}
	if err := module.Provision(caddy.Context{}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if module.storage.base != "http://127.0.0.1:9999" {
		t.Errorf("base = %q, want the expanded endpoint", module.storage.base)
	}
	if module.storage.token != "tok" {
		t.Errorf("token = %q, want %q", module.storage.token, "tok")
	}
}

func TestCertMagicStorageBeforeProvision(t *testing.T) {
	module := &Module{}
	if _, err := module.CertMagicStorage(); err == nil {
		t.Fatal("CertMagicStorage error = nil, want an error before provisioning")
	}
}

func TestCaddyModuleID(t *testing.T) {
	info := Module{}.CaddyModule()
	if info.ID != "caddy.storage.scafo" {
		t.Errorf("module ID = %q, want %q", info.ID, "caddy.storage.scafo")
	}
	if _, ok := info.New().(*Module); !ok {
		t.Errorf("New() returned %T, want *Module", info.New())
	}
}

func TestUnmarshalCaddyfile(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		wantEndpoint  string
		wantTokenFile string
		wantErr       string
	}{
		{
			name: "full block",
			input: `scafo {
				endpoint http://127.0.0.1:7400
				token_file /var/lib/scafo/storage-token
			}`,
			wantEndpoint:  "http://127.0.0.1:7400",
			wantTokenFile: "/var/lib/scafo/storage-token",
		},
		{
			name: "empty block",
			input: `scafo {
			}`,
		},
		{
			name: "unknown option",
			input: `scafo {
				bucket my-bucket
			}`,
			wantErr: "unrecognized scafo storage option",
		},
		{
			name: "missing argument",
			input: `scafo {
				endpoint
			}`,
			wantErr: "wrong argument count",
		},
		{
			name: "too many arguments",
			input: `scafo {
				endpoint http://a http://b
			}`,
			wantErr: "wrong argument count",
		},
		{
			name:    "inline argument is rejected",
			input:   `scafo bogus`,
			wantErr: "wrong argument count",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var module Module
			err := module.UnmarshalCaddyfile(caddyfile.NewTestDispenser(tt.input))
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("UnmarshalCaddyfile error = nil, want %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %q, want it to mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("UnmarshalCaddyfile: %v", err)
			}
			if module.Endpoint != tt.wantEndpoint {
				t.Errorf("Endpoint = %q, want %q", module.Endpoint, tt.wantEndpoint)
			}
			if module.TokenFile != tt.wantTokenFile {
				t.Errorf("TokenFile = %q, want %q", module.TokenFile, tt.wantTokenFile)
			}
		})
	}
}
