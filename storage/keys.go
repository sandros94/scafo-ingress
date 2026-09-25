package storage

import (
	"strings"
)

// Certificates must not cross fleets. One Caddy in front of several
// installations holds one CertMagic and therefore one keyspace, and every key
// in it that names a domain has to end up in the bucket of the fleet that owns
// that domain -- through that fleet's node and that fleet's token, so that a
// compromised or misconfigured fleet cannot read another's private keys.
//
// This file is the whole of that decision: given a storage key, which of the
// three places does it belong to?
//
//   - scopeOwned -- the key names a domain, and goes to the fleet that owns
//     that domain.
//   - scopeLocal -- the key names no domain: the ACME account, CertMagic's
//     housekeeping, Caddy's own PKI and ECH keys. There is no "home fleet" to
//     put those in, so they live on the ingress's own disk.
//   - scopeFanout -- the key is a pure prefix whose CHILDREN name domains.
//     Only a listing is meaningful on one, and a listing of it has to ask
//     everyone.
//
// The key shapes below are CertMagic's, enumerated from its source at the
// version in go.mod (v0.25.3):
//
//	certificates/<issuer>/<domain>/<domain>.{crt,key,json}   KeyBuilder.Site*
//	ocsp/<domain>-<hash>                                     KeyBuilder.OCSPStaple
//	acme/<issuer>/challenge_tokens/<domain>.json             distributedSolver
//	acme/<issuer>/users/<email>/{registration.json,private.key}   ACMEIssuer
//	last_clean.json                                          CleanStorage
//	rw_test_<random>                                         Config.checkStorage
//
// and these lock names:
//
//	issue_cert_<domain>          Config.lockKey       -- domain-bearing
//	ari_<unique-id>              renewal ARI fetch    -- names no domain
//	register_acme_account[_<contact>]                 -- names no domain
//	storage_clean                                     -- names no domain
//
// Every component but a lock name has been through KeyBuilder.Safe, which
// lowercases, replaces "*" with "wildcard_" and drops anything outside
// [\w@.-]. A DNS name survives that unchanged, so the only inverse this needs
// is the wildcard one; see unsafeDomain.

// scope is where a key belongs.
type scope uint8

const (
	// scopeLocal is the ingress's own disk.
	scopeLocal scope = iota

	// scopeOwned is the fleet that owns the domain the key names.
	scopeOwned

	// scopeFanout is a prefix whose children are domain-bearing: every fleet,
	// and the local disk, may hold something under it.
	scopeFanout
)

// placement is where one key belongs, and which domain decided it.
type placement struct {
	scope scope

	// domain is the name the key belongs to, in the form the routing document
	// uses -- so "*.example.com" and not "wildcard_.example.com". It is set
	// only for scopeOwned.
	domain string
}

// prefixes of the CertMagic keyspace this package knows by name.
const (
	prefixCerts     = "certificates"
	prefixOCSP      = "ocsp"
	prefixACME      = "acme"
	segChallenges   = "challenge_tokens"
	segUsers        = "users"
	lockIssueCert   = "issue_cert_"
	wildcardSafe    = "wildcard_."
	challengeSuffix = ".json"
)

// classify decides where a storage key belongs.
func classify(key string) placement {
	key = strings.Trim(key, "/")
	if key == "" {
		// The root. Only a listing reaches it, and a listing of the root has
		// to ask everyone.
		return placement{scope: scopeFanout}
	}
	parts := strings.Split(key, "/")

	switch parts[0] {
	case prefixCerts:
		// certificates            -> issuers below it
		// certificates/<issuer>   -> domains below it
		// certificates/<issuer>/<domain>[/...]
		if len(parts) <= 2 {
			return placement{scope: scopeFanout}
		}
		return placement{scope: scopeOwned, domain: unsafeDomain(parts[2])}

	case prefixOCSP:
		// ocsp            -> staples below it
		// ocsp/<domain>-<hash>
		if len(parts) == 1 {
			return placement{scope: scopeFanout}
		}
		if domain, ok := ocspDomain(parts[1]); ok {
			return placement{scope: scopeOwned, domain: domain}
		}
		// A staple whose name carries no domain -- CertMagic writes one when
		// the certificate has no names at all -- belongs to nobody. Treating
		// it as unowned is what keeps it out of every fleet's bucket.
		return placement{scope: scopeOwned}

	case prefixACME:
		// acme                                   -> issuers below it
		// acme/<issuer>                          -> users and challenge tokens
		// acme/<issuer>/users/...                -> the ACME account
		// acme/<issuer>/challenge_tokens/<d>.json
		if len(parts) <= 2 {
			return placement{scope: scopeFanout}
		}
		switch parts[2] {
		case segUsers:
			return placement{scope: scopeLocal}
		case segChallenges:
			if len(parts) == 3 {
				return placement{scope: scopeFanout}
			}
			name := strings.TrimSuffix(parts[3], challengeSuffix)
			return placement{scope: scopeOwned, domain: unsafeDomain(name)}
		}
		return placement{scope: scopeLocal}
	}

	// Everything else -- last_clean.json, rw_test_*, Caddy's pki/, ech/ and
	// stek/ -- names no domain and has no fleet to belong to.
	return placement{scope: scopeLocal}
}

// classifyLock decides where a lock belongs. Only the certificate-issuance
// lock names a domain; the rest coordinate work that is the ingress's own.
//
// The ARI lock is the interesting omission: it is named after a certificate's
// ARI unique identifier, which carries no domain at all, so it cannot be
// routed to a fleet and is taken locally. That makes it a lock between the
// goroutines of THIS Caddy rather than between several of them, which is what
// it was already for anyone running a single ingress.
func classifyLock(name string) placement {
	if domain, found := strings.CutPrefix(name, lockIssueCert); found && domain != "" {
		return placement{scope: scopeOwned, domain: strings.ToLower(domain)}
	}
	return placement{scope: scopeLocal}
}

// ocspDomain reads the domain out of an OCSP staple's key component, which
// CertMagic builds as `<safe first name>-<hash of the PEM bundle>`. The hash
// is the tail after the LAST hyphen, because a domain may contain hyphens of
// its own and the hash never does anything but end the name.
func ocspDomain(component string) (string, bool) {
	cut := strings.LastIndex(component, "-")
	if cut <= 0 {
		return "", false
	}
	return unsafeDomain(component[:cut]), true
}

// unsafeDomain undoes the one lossy thing CertMagic's key sanitizer does to a
// DNS name: it writes a wildcard's "*" as "wildcard_". Everything else in a
// hostname -- letters, digits, hyphens, dots -- it leaves alone, so there is
// nothing else to undo.
func unsafeDomain(component string) string {
	if base, found := strings.CutPrefix(component, wildcardSafe); found && base != "" {
		return "*." + base
	}
	return component
}
