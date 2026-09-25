package permission

import (
	"context"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

// Which names are allowed is decided in the fleets package, against real
// routing documents, and tested there. What is left here is this wrapper's
// identity, its Caddyfile form and its refusal before it is provisioned.

func TestCaddyModuleID(t *testing.T) {
	if got := (Permission{}).CaddyModule().ID; got != "tls.permission.scafo" {
		t.Errorf("module ID = %q", got)
	}
}

func TestUnmarshalCaddyfile(t *testing.T) {
	var permission Permission
	if err := permission.UnmarshalCaddyfile(caddyfile.NewTestDispenser("scafo")); err != nil {
		t.Fatalf("UnmarshalCaddyfile: %v", err)
	}
	if err := permission.UnmarshalCaddyfile(caddyfile.NewTestDispenser("scafo extra")); err == nil {
		t.Error("error = nil for an argument this module does not take")
	}
}

func TestCertificateAllowedBeforeProvision(t *testing.T) {
	// A module with no app allows nothing. "I do not know" is not "yes": a
	// certificate is not obtained for whatever happens to connect first.
	var permission Permission
	err := permission.CertificateAllowed(context.Background(), "app.example")
	if err == nil {
		t.Fatal("error = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "not been provisioned") {
		t.Errorf("error = %q, want it to say the module is not provisioned", err)
	}
}
