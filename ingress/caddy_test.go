package ingress

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"

	"github.com/sandros94/scafo-ingress/internal/nodeapi"
)

// requestFor builds a request carrying the variables map Caddy puts in every
// request context, which is where the module writes its outcome.
func requestFor(t *testing.T, host, target string) (*http.Request, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = host
	vars := make(map[string]any)
	return req.WithContext(context.WithValue(req.Context(), caddyhttp.VarsCtxKey, vars)), vars
}

// provisionedModule returns a module already holding doc, without a poller or
// a daemon behind it.
func provisionedModule(t *testing.T, raw string) *Module {
	t.Helper()
	module := &Module{routes: NewRoutes(nil, "", DefaultPrefer, nil)}
	if raw != "" {
		module.routes.current.Store(parseDocument(t, raw))
	}
	return module
}

func TestGetUpstreams(t *testing.T) {
	module := provisionedModule(t, testDocument)

	tests := []struct {
		name        string
		host        string
		target      string
		wantDials   []string
		wantOutcome Outcome
		wantHost    string
	}{
		{
			name:        "route",
			host:        "app.dev.example",
			target:      "/",
			wantDials:   []string{"10.83.0.1:41002", "10.83.0.2:41002"},
			wantOutcome: OutcomeRoute,
			wantHost:    "app.dev.example",
		},
		{
			// The Host header carries a port whenever the listener is not on
			// 80 or 443, and its case is the client's to choose.
			name:        "route, host with a port and mixed case",
			host:        "APP.Dev.Example:8443",
			target:      "/",
			wantDials:   []string{"10.83.0.1:41002", "10.83.0.2:41002"},
			wantOutcome: OutcomeRoute,
			wantHost:    "app.dev.example",
		},
		{
			name:        "api host",
			host:        "dev.example",
			target:      "/v1/projects",
			wantDials:   []string{"127.0.0.1:7400"},
			wantOutcome: OutcomeRoute,
			wantHost:    "dev.example",
		},
		{
			name:        "api host, denied path",
			host:        "dev.example",
			target:      "/v1/storage/objects/x",
			wantOutcome: OutcomeDenied,
			wantHost:    "dev.example",
		},
		{
			name:        "unknown host",
			host:        "nobody.example",
			target:      "/",
			wantOutcome: OutcomeUnknown,
			wantHost:    "nobody.example",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, vars := requestFor(t, tt.host, tt.target)
			upstreams, err := module.GetUpstreams(req)
			// Refusing is never an error: an error would make reverse_proxy
			// log at error level and fall back to its static upstreams, which
			// would hand an unknown host a way to fill the log.
			if err != nil {
				t.Fatalf("GetUpstreams: %v", err)
			}
			if got := dials(upstreams); strings.Join(got, ",") != strings.Join(tt.wantDials, ",") {
				t.Errorf("dials = %v, want %v", got, tt.wantDials)
			}
			if got := vars[RouteVar]; got != string(tt.wantOutcome) {
				t.Errorf("%s = %v, want %q", RouteVar, got, tt.wantOutcome)
			}
			if got := vars[HostVar]; got != tt.wantHost {
				t.Errorf("%s = %v, want %q", HostVar, got, tt.wantHost)
			}
		})
	}
}

func TestGetUpstreamsWithoutADocument(t *testing.T) {
	module := provisionedModule(t, "")
	req, vars := requestFor(t, "app.dev.example", "/")

	upstreams, err := module.GetUpstreams(req)
	if err != nil {
		t.Fatalf("GetUpstreams: %v", err)
	}
	if len(upstreams) != 0 {
		t.Errorf("upstreams = %v, want none", dials(upstreams))
	}
	// Distinct from "unknown" on purpose: a cold start is not a missing app,
	// and an operator turning unknown hosts into 404 must not catch this too.
	if got := vars[RouteVar]; got != string(OutcomeUnavailable) {
		t.Errorf("%s = %v, want %q", RouteVar, got, OutcomeUnavailable)
	}
}

func TestGetUpstreamsBeforeProvision(t *testing.T) {
	module := &Module{}
	req, _ := requestFor(t, "app.dev.example", "/")
	if _, err := module.GetUpstreams(req); err == nil {
		t.Fatal("GetUpstreams error = nil, want an error before provisioning")
	}
}

func TestProvisionDefaults(t *testing.T) {
	// The daemon is not answering here, which is the point: Caddy must come up
	// anyway, with the defaults in place and the poller running.
	module := &Module{Config: nodeapi.Config{
		Endpoint: "http://127.0.0.1:1",
		Token:    "tok",
	}}
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()

	if err := module.Provision(ctx); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if module.routes == nil {
		t.Fatal("routes = nil after Provision")
	}
	if module.routes.prefer != DefaultPrefer {
		t.Errorf("prefer = %q, want %q", module.routes.prefer, DefaultPrefer)
	}
	if module.routes.Document() != nil {
		t.Error("Document is set although the daemon never answered")
	}
	if err := module.Cleanup(); err != nil {
		t.Errorf("Cleanup: %v", err)
	}
}

func TestProvisionFetchesOnce(t *testing.T) {
	server := newRoutesServer(t, testDocument)
	module := &Module{
		Config:       nodeapi.Config{Endpoint: server.URL, Token: "test-token"},
		PollInterval: caddy.Duration(time.Hour),
	}
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()

	if err := module.Provision(ctx); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	defer func() {
		if err := module.Cleanup(); err != nil {
			t.Errorf("Cleanup: %v", err)
		}
	}()

	// A healthy pair starts already routing rather than answering the first
	// requests from an empty document.
	req, vars := requestFor(t, "app.dev.example", "/")
	upstreams, err := module.GetUpstreams(req)
	if err != nil {
		t.Fatalf("GetUpstreams: %v", err)
	}
	if got := dials(upstreams); strings.Join(got, ",") != "10.83.0.1:41002,10.83.0.2:41002" {
		t.Errorf("dials = %v, want the document's upstreams", got)
	}
	if got := vars[RouteVar]; got != string(OutcomeRoute) {
		t.Errorf("%s = %v, want %q", RouteVar, got, OutcomeRoute)
	}
}

func TestProvisionExpandsPlaceholders(t *testing.T) {
	t.Setenv("SCAFO_TEST_PREFER", "vps-b")
	module := &Module{
		Config: nodeapi.Config{Endpoint: "http://127.0.0.1:1", Token: "tok"},
		Prefer: "{env.SCAFO_TEST_PREFER}",
	}
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()

	if err := module.Provision(ctx); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if module.routes.prefer != "vps-b" {
		t.Errorf("prefer = %q, want the expanded value", module.routes.prefer)
	}
}

func TestProvisionKeepsPreferNone(t *testing.T) {
	// Only an unset value takes the default. "none" is a stated preference --
	// for no preference -- and has to survive as far as the request, where it
	// becomes an absent parameter rather than "self".
	module := &Module{
		Config: nodeapi.Config{Endpoint: "http://127.0.0.1:1", Token: "tok"},
		Prefer: PreferNone,
	}
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()

	if err := module.Provision(ctx); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if module.routes.prefer != PreferNone {
		t.Errorf("prefer = %q, want %q", module.routes.prefer, PreferNone)
	}
	if got := module.routes.requestURL(); strings.Contains(got, "prefer") {
		t.Errorf("requestURL = %q, want no prefer parameter", got)
	}
}

func TestProvisionErrors(t *testing.T) {
	tests := []struct {
		name    string
		module  Module
		wantErr string
	}{
		{
			name:    "no endpoint",
			module:  Module{Config: nodeapi.Config{Token: "tok"}},
			wantErr: "endpoint is required",
		},
		{
			name:    "no token",
			module:  Module{Config: nodeapi.Config{Endpoint: "http://127.0.0.1:7400"}},
			wantErr: "either token or token_file is required",
		},
		{
			name: "negative poll interval",
			module: Module{
				Config:       nodeapi.Config{Endpoint: "http://127.0.0.1:7400", Token: "tok"},
				PollInterval: caddy.Duration(-time.Second),
			},
			wantErr: "poll_interval must be positive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			module := tt.module
			ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
			defer cancel()

			err := module.Provision(ctx)
			if err == nil {
				t.Fatalf("Provision error = nil, want an error mentioning %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Provision error = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestProvisionFromJSONConfig(t *testing.T) {
	// A JSON configuration never passes through the Caddyfile parser, so the
	// struct tags are the whole contract for it.
	t.Setenv("SCAFO_STORAGE_TOKEN", "json-token")

	const config = `{
		"source": "scafo",
		"endpoint": "http://127.0.0.1:7400/prefix",
		"token": "{env.SCAFO_STORAGE_TOKEN}",
		"prefer": "vps-b",
		"poll_interval": "10s"
	}`

	var module Module
	if err := json.Unmarshal([]byte(config), &module); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if module.Prefer != "vps-b" {
		t.Errorf("Prefer = %q, want %q", module.Prefer, "vps-b")
	}
	if time.Duration(module.PollInterval) != 10*time.Second {
		t.Errorf("PollInterval = %s, want 10s", time.Duration(module.PollInterval))
	}

	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()
	if err := module.Provision(ctx); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if module.routes.token != "json-token" {
		t.Errorf("token = %q, want %q", module.routes.token, "json-token")
	}
	if module.routes.base != "http://127.0.0.1:7400/prefix" {
		t.Errorf("base = %q, want the endpoint with its path prefix", module.routes.base)
	}
}

func TestCaddyModuleID(t *testing.T) {
	info := Module{}.CaddyModule()
	if info.ID != "http.reverse_proxy.upstreams.scafo" {
		t.Errorf("module ID = %q, want %q", info.ID, "http.reverse_proxy.upstreams.scafo")
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
		wantToken     string
		wantTokenFile string
		wantPrefer    string
		wantInterval  time.Duration
		wantErr       string
	}{
		{
			name: "full block",
			input: `scafo {
				endpoint      http://127.0.0.1:7400
				token_file    /var/lib/scafo/storage-token
				prefer        vps-b
				poll_interval 30s
			}`,
			wantEndpoint:  "http://127.0.0.1:7400",
			wantTokenFile: "/var/lib/scafo/storage-token",
			wantPrefer:    "vps-b",
			wantInterval:  30 * time.Second,
		},
		{
			// Everything but the connection is optional, and what is left out
			// stays zero here; Provision is where the defaults are applied.
			name: "connection only",
			input: `scafo {
				endpoint http://127.0.0.1:7400
				token    supersecret
			}`,
			wantEndpoint: "http://127.0.0.1:7400",
			wantToken:    "supersecret",
		},
		{
			name: "placeholders are stored verbatim",
			input: `scafo {
				endpoint {env.SCAFO_ENDPOINT}
				token    {env.SCAFO_STORAGE_TOKEN}
				prefer   {env.SCAFO_PREFER}
			}`,
			wantEndpoint: "{env.SCAFO_ENDPOINT}",
			wantToken:    "{env.SCAFO_STORAGE_TOKEN}",
			wantPrefer:   "{env.SCAFO_PREFER}",
		},
		{
			// Caddy's own duration parser, so a day is a unit here too.
			name: "poll_interval in caddy's duration syntax",
			input: `scafo {
				poll_interval 1500ms
			}`,
			wantInterval: 1500 * time.Millisecond,
		},
		{
			name: "empty block",
			input: `scafo {
			}`,
		},
		{
			name: "unparseable poll_interval",
			input: `scafo {
				poll_interval soon
			}`,
			wantErr: "parsing poll_interval",
		},
		{
			// Written down, so it cannot be read as "use the default".
			name: "zero poll_interval",
			input: `scafo {
				poll_interval 0
			}`,
			wantErr: "poll_interval must be positive",
		},
		{
			name: "negative poll_interval",
			input: `scafo {
				poll_interval -5s
			}`,
			wantErr: "poll_interval must be positive",
		},
		{
			name: "prefer without an argument",
			input: `scafo {
				prefer
			}`,
			wantErr: "wrong argument count",
		},
		{
			name: "prefer with too many arguments",
			input: `scafo {
				prefer self vps-b
			}`,
			wantErr: "wrong argument count",
		},
		{
			name: "unknown option",
			input: `scafo {
				lb_policy first
			}`,
			wantErr: "unrecognized scafo ingress option",
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
			if module.Token != tt.wantToken {
				t.Errorf("Token = %q, want %q", module.Token, tt.wantToken)
			}
			if module.TokenFile != tt.wantTokenFile {
				t.Errorf("TokenFile = %q, want %q", module.TokenFile, tt.wantTokenFile)
			}
			if module.Prefer != tt.wantPrefer {
				t.Errorf("Prefer = %q, want %q", module.Prefer, tt.wantPrefer)
			}
			if time.Duration(module.PollInterval) != tt.wantInterval {
				t.Errorf("PollInterval = %s, want %s", time.Duration(module.PollInterval), tt.wantInterval)
			}
		})
	}
}

func TestCleanupBeforeProvision(t *testing.T) {
	module := &Module{}
	if err := module.Cleanup(); err != nil {
		t.Errorf("Cleanup: %v", err)
	}
}
