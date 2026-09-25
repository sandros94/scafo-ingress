package storage

import (
	"testing"

	"github.com/caddyserver/certmagic"
)

// TestClassifyEveryCertMagicKeyShape walks the key shapes CertMagic and Caddy
// actually build, asking of each one: does it name a domain, and which?
//
// The keys are built with CertMagic's own KeyBuilder wherever it offers one,
// so that a change in how it sanitizes or lays out a key breaks this test
// rather than quietly sending a certificate to the wrong fleet.
func TestClassifyEveryCertMagicKeyShape(t *testing.T) {
	const issuer = "acme-v02.api.letsencrypt.org-directory"

	tests := []struct {
		name   string
		key    string
		scope  scope
		domain string
	}{
		{
			name:  "the certificates prefix lists issuers",
			key:   "certificates",
			scope: scopeFanout,
		},
		{
			name:  "an issuer's prefix lists domains",
			key:   certmagic.StorageKeys.CertsPrefix(issuer),
			scope: scopeFanout,
		},
		{
			name:   "a site's prefix names its domain",
			key:    certmagic.StorageKeys.CertsSitePrefix(issuer, "app.example.com"),
			scope:  scopeOwned,
			domain: "app.example.com",
		},
		{
			name:   "a certificate names its domain",
			key:    certmagic.StorageKeys.SiteCert(issuer, "app.example.com"),
			scope:  scopeOwned,
			domain: "app.example.com",
		},
		{
			name:   "a private key names its domain",
			key:    certmagic.StorageKeys.SitePrivateKey(issuer, "app.example.com"),
			scope:  scopeOwned,
			domain: "app.example.com",
		},
		{
			name:   "certificate metadata names its domain",
			key:    certmagic.StorageKeys.SiteMeta(issuer, "app.example.com"),
			scope:  scopeOwned,
			domain: "app.example.com",
		},
		{
			name:   "a wildcard certificate's key names the wildcard",
			key:    certmagic.StorageKeys.SiteCert(issuer, "*.example.com"),
			scope:  scopeOwned,
			domain: "*.example.com",
		},
		{
			name:  "the ocsp prefix lists staples",
			key:   "ocsp",
			scope: scopeFanout,
		},
		{
			name:   "an OCSP staple names the certificate's first domain",
			key:    "ocsp/app.example.com-1a2b3c4d",
			scope:  scopeOwned,
			domain: "app.example.com",
		},
		{
			name:   "an OCSP staple for a name with hyphens keeps them",
			key:    "ocsp/my-app.example.com-1a2b3c4d",
			scope:  scopeOwned,
			domain: "my-app.example.com",
		},
		{
			name:   "an OCSP staple for a wildcard names the wildcard",
			key:    "ocsp/wildcard_.example.com-1a2b3c4d",
			scope:  scopeOwned,
			domain: "*.example.com",
		},
		{
			name:  "an OCSP staple with no name in it belongs to nobody",
			key:   "ocsp/1a2b3c4d",
			scope: scopeOwned,
		},
		{
			name:  "the acme prefix lists issuers",
			key:   "acme",
			scope: scopeFanout,
		},
		{
			name:  "an acme issuer's prefix holds both users and challenge tokens",
			key:   "acme/" + issuer,
			scope: scopeFanout,
		},
		{
			name:  "the ACME account registration is the ingress's own",
			key:   "acme/" + issuer + "/users/ops@operator.example/registration.json",
			scope: scopeLocal,
		},
		{
			name:  "the ACME account private key is the ingress's own",
			key:   "acme/" + issuer + "/users/ops@operator.example/private.key",
			scope: scopeLocal,
		},
		{
			name:  "the challenge token prefix lists domains",
			key:   "acme/" + issuer + "/challenge_tokens",
			scope: scopeFanout,
		},
		{
			name:   "a challenge token names its domain",
			key:    "acme/" + issuer + "/challenge_tokens/app.example.com.json",
			scope:  scopeOwned,
			domain: "app.example.com",
		},
		{
			name:   "a challenge token for a wildcard names the wildcard",
			key:    "acme/" + issuer + "/challenge_tokens/wildcard_.example.com.json",
			scope:  scopeOwned,
			domain: "*.example.com",
		},
		{
			name:  "the storage cleaning timestamp is housekeeping",
			key:   "last_clean.json",
			scope: scopeLocal,
		},
		{
			name:  "CertMagic's storage check names no domain",
			key:   "rw_test_123456789",
			scope: scopeLocal,
		},
		{
			name:  "Caddy's own PKI is not a fleet's",
			key:   "pki/authorities/local/root.crt",
			scope: scopeLocal,
		},
		{
			name:  "Caddy's ECH keys are not a fleet's",
			key:   "ech/abcdef/key.bin",
			scope: scopeLocal,
		},
		{
			name:  "an empty key is the root",
			key:   "",
			scope: scopeFanout,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := classify(test.key)
			if got.scope != test.scope {
				t.Fatalf("classify(%q).scope = %v, want %v", test.key, got.scope, test.scope)
			}
			if got.domain != test.domain {
				t.Errorf("classify(%q).domain = %q, want %q", test.key, got.domain, test.domain)
			}
		})
	}
}

// TestClassifyLock covers every lock name CertMagic takes.
func TestClassifyLock(t *testing.T) {
	tests := []struct {
		name   string
		lock   string
		scope  scope
		domain string
	}{
		{
			name:   "the issuance lock names its domain",
			lock:   "issue_cert_app.example.com",
			scope:  scopeOwned,
			domain: "app.example.com",
		},
		{
			name:   "the issuance lock for a wildcard names the wildcard",
			lock:   "issue_cert_*.example.com",
			scope:  scopeOwned,
			domain: "*.example.com",
		},
		{
			name:  "the ARI lock names a certificate, not a domain",
			lock:  "ari_aYhba4dGQKicMQjGRjnbHw.wnDFPjPrxo3k",
			scope: scopeLocal,
		},
		{
			name:  "the account registration lock names no domain",
			lock:  "register_acme_account_ops@operator.example",
			scope: scopeLocal,
		},
		{
			name:  "the storage cleaning lock names no domain",
			lock:  "storage_clean",
			scope: scopeLocal,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := classifyLock(test.lock)
			if got.scope != test.scope || got.domain != test.domain {
				t.Errorf("classifyLock(%q) = %v/%q, want %v/%q", test.lock, got.scope, got.domain, test.scope, test.domain)
			}
		})
	}
}

// TestSanitizedDomainsRoundTrip pins the one lossy transformation this package
// has to undo. If CertMagic changes how it writes an asterisk, the wildcard
// certificates start going to the wrong place, so break here instead.
func TestSanitizedDomainsRoundTrip(t *testing.T) {
	for _, domain := range []string{
		"app.example.com",
		"my-app.example.com",
		"*.example.com",
		"*.apps.example.com",
	} {
		safe := certmagic.StorageKeys.Safe(domain)
		if got := unsafeDomain(safe); got != domain {
			t.Errorf("unsafeDomain(Safe(%q)) = %q, want %q", domain, got, domain)
		}
	}
	if got := certmagic.StorageKeys.Safe("*.example.com"); got != "wildcard_.example.com" {
		t.Errorf("CertMagic now sanitizes a wildcard as %q; keys.go assumes %q", got, "wildcard_.example.com")
	}
}
