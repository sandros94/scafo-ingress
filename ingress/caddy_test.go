package ingress

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

// The resolution these modules perform lives in the fleets package and is
// tested there, against real routing documents. What is left here is what
// these two wrappers own: their module identity, their Caddyfile form -- which
// now takes no options at all -- and what they do before they are provisioned.

func TestCaddyModuleIDs(t *testing.T) {
	if got := (Module{}).CaddyModule().ID; got != "http.reverse_proxy.upstreams.scafo" {
		t.Errorf("upstreams module ID = %q", got)
	}
	if got := (MatchPlainHTTP{}).CaddyModule().ID; got != "http.matchers.scafo_plain_http" {
		t.Errorf("matcher module ID = %q", got)
	}
}

func TestUnmarshalCaddyfile(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr string
	}{
		{name: "the bare directive", input: "scafo"},
		{name: "an empty block", input: "scafo {\n}"},
		{
			name:    "an inline argument",
			input:   "scafo self",
			wantErr: "wrong argument count",
		},
		{
			// The old per-directive connection options are gone, with no
			// compatibility shim: a fleet is stated once, in the scafo app.
			name:    "the old endpoint option",
			input:   "scafo {\n\tendpoint http://127.0.0.1:7400\n}",
			wantErr: "fleets are configured in the scafo global option",
		},
		{
			name:    "the old token option",
			input:   "scafo {\n\ttoken t\n}",
			wantErr: "fleets are configured in the scafo global option",
		},
		{
			name:    "the old prefer option",
			input:   "scafo {\n\tprefer self\n}",
			wantErr: "fleets are configured in the scafo global option",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var module Module
			err := module.UnmarshalCaddyfile(caddyfile.NewTestDispenser(test.input))
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("UnmarshalCaddyfile: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("error = nil, want one mentioning %q", test.wantErr)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, test.wantErr)
			}
		})
	}
}

func TestMatcherUnmarshalCaddyfile(t *testing.T) {
	var matcher MatchPlainHTTP
	if err := matcher.UnmarshalCaddyfile(caddyfile.NewTestDispenser("scafo_plain_http")); err != nil {
		t.Fatalf("UnmarshalCaddyfile: %v", err)
	}
	if err := matcher.UnmarshalCaddyfile(caddyfile.NewTestDispenser("scafo_plain_http extra")); err == nil {
		t.Error("error = nil for an argument this matcher does not take")
	}
}

func TestGetUpstreamsBeforeProvision(t *testing.T) {
	var module Module
	if _, err := module.GetUpstreams(httptest.NewRequest(http.MethodGet, "/", nil)); err == nil {
		t.Fatal("error = nil, want one saying the module is not provisioned")
	}
}

func TestMatchBeforeProvision(t *testing.T) {
	// A matcher with no app matches nothing, which is the safe answer: the
	// default is the redirect, and only a fleet that asked for plain HTTP
	// gets it.
	var matcher MatchPlainHTTP
	if matcher.Match(httptest.NewRequest(http.MethodGet, "/", nil)) {
		t.Error("an unprovisioned matcher matched; the safe default is not to")
	}
}
