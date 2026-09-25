package storage

import (
	"context"
	"errors"
	"io/fs"
	"sort"
	"strings"
	"testing"

	"github.com/caddyserver/certmagic"
)

// memStore is an in-memory certmagic.Storage, so that a test can see exactly
// which store a key reached.
type memStore struct {
	name  string
	items map[string][]byte
	locks map[string]struct{}
}

func newMemStore(name string) *memStore {
	return &memStore{name: name, items: map[string][]byte{}, locks: map[string]struct{}{}}
}

func (m *memStore) Store(_ context.Context, key string, value []byte) error {
	m.items[key] = value
	return nil
}

func (m *memStore) Load(_ context.Context, key string) ([]byte, error) {
	value, ok := m.items[key]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return value, nil
}

func (m *memStore) Delete(_ context.Context, key string) error {
	if _, ok := m.items[key]; !ok {
		return fs.ErrNotExist
	}
	delete(m.items, key)
	return nil
}

func (m *memStore) Exists(_ context.Context, key string) bool {
	_, ok := m.items[key]
	return ok
}

func (m *memStore) List(_ context.Context, prefix string, _ bool) ([]string, error) {
	var keys []string
	for key := range m.items {
		if key == prefix || strings.HasPrefix(key, prefix+"/") {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return nil, fs.ErrNotExist
	}
	sort.Strings(keys)
	return keys, nil
}

func (m *memStore) Stat(_ context.Context, key string) (certmagic.KeyInfo, error) {
	value, ok := m.items[key]
	if !ok {
		return certmagic.KeyInfo{}, fs.ErrNotExist
	}
	return certmagic.KeyInfo{Key: key, Size: int64(len(value)), IsTerminal: true}, nil
}

func (m *memStore) Lock(_ context.Context, name string) error {
	m.locks[name] = struct{}{}
	return nil
}

func (m *memStore) Unlock(_ context.Context, name string) error {
	delete(m.locks, name)
	return nil
}

// testRouter builds a router over two fleets and a local disk, with a fixed
// idea of who owns what.
func testRouter() (*Router, map[string]*memStore) {
	stores := map[string]*memStore{
		"local": newMemStore("local"),
		"a":     newMemStore("a"),
		"b":     newMemStore("b"),
	}
	owners := map[string]string{
		"app.a.example":    "a",
		"*.a.example":      "a",
		"app.b.example":    "b",
		"*.b.example":      "b",
		"my-app.a.example": "a",
	}
	owner := func(domain string) (string, bool) {
		fleet, ok := owners[domain]
		return fleet, ok
	}
	router := newRouter(owner, []string{"a", "b"}, map[string]certmagic.Storage{
		"a": stores["a"],
		"b": stores["b"],
	}, stores["local"])
	return router, stores
}

const testIssuer = "acme-v02.api.letsencrypt.org-directory"

func TestAKeyGoesToTheFleetThatOwnsItsDomain(t *testing.T) {
	router, stores := testRouter()
	ctx := context.Background()

	keyA := certmagic.StorageKeys.SiteCert(testIssuer, "app.a.example")
	keyB := certmagic.StorageKeys.SiteCert(testIssuer, "app.b.example")
	if err := router.Store(ctx, keyA, []byte("cert-a")); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if err := router.Store(ctx, keyB, []byte("cert-b")); err != nil {
		t.Fatalf("Store: %v", err)
	}

	if _, ok := stores["a"].items[keyA]; !ok {
		t.Error("fleet a's certificate did not reach fleet a")
	}
	if _, ok := stores["b"].items[keyA]; ok {
		t.Error("fleet a's certificate reached fleet b; certificates must not cross fleets")
	}
	if _, ok := stores["local"].items[keyA]; ok {
		t.Error("fleet a's certificate reached the local disk")
	}

	value, err := router.Load(ctx, keyB)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(value) != "cert-b" {
		t.Errorf("Load = %q, want %q", value, "cert-b")
	}
}

func TestEveryKeyShapeReachesTheRightPlace(t *testing.T) {
	router, stores := testRouter()
	ctx := context.Background()

	tests := []struct {
		name  string
		key   string
		store string
	}{
		{"a certificate", certmagic.StorageKeys.SiteCert(testIssuer, "app.a.example"), "a"},
		{"a private key", certmagic.StorageKeys.SitePrivateKey(testIssuer, "app.b.example"), "b"},
		{"certificate metadata", certmagic.StorageKeys.SiteMeta(testIssuer, "app.a.example"), "a"},
		{"a wildcard certificate", certmagic.StorageKeys.SiteCert(testIssuer, "*.b.example"), "b"},
		{"an OCSP staple", "ocsp/app.a.example-1a2b3c4d", "a"},
		{"a wildcard's OCSP staple", "ocsp/wildcard_.b.example-1a2b3c4d", "b"},
		{"a challenge token", "acme/" + testIssuer + "/challenge_tokens/app.a.example.json", "a"},
		{"the ACME account", "acme/" + testIssuer + "/users/ops@x.example/private.key", "local"},
		{"the housekeeping timestamp", "last_clean.json", "local"},
		{"CertMagic's storage check", "rw_test_42", "local"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := router.Store(ctx, test.key, []byte("v")); err != nil {
				t.Fatalf("Store: %v", err)
			}
			for name, store := range stores {
				_, present := store.items[test.key]
				if present != (name == test.store) {
					t.Errorf("key %q is %v in store %q, want present only in %q",
						test.key, present, name, test.store)
				}
			}
			// Clean up so the next case starts from an empty keyspace.
			_ = router.Delete(ctx, test.key)
		})
	}
}

func TestAnUnownedDomainIsMissingOnReadAndRefusedOnWrite(t *testing.T) {
	router, _ := testRouter()
	ctx := context.Background()
	key := certmagic.StorageKeys.SiteCert(testIssuer, "stranger.example")

	if _, err := router.Load(ctx, key); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Load error = %v, want it to wrap fs.ErrNotExist", err)
	}
	if _, err := router.Stat(ctx, key); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stat error = %v, want it to wrap fs.ErrNotExist", err)
	}
	if router.Exists(ctx, key) {
		t.Error("Exists = true for a domain no fleet serves")
	}
	err := router.Store(ctx, key, []byte("v"))
	if err == nil {
		t.Fatal("Store error = nil; a key nobody owns must not be written anywhere")
	}
	if !strings.Contains(err.Error(), "stranger.example") {
		t.Errorf("Store error = %q, want it to name the domain", err)
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Error("a refused write reads as a missing key; it is a failure, not an absence")
	}
	if err := router.Delete(ctx, key); err == nil {
		t.Error("Delete error = nil; a key nobody owns must not be deleted through a guess")
	}
}

func TestListFansOutAndFiltersByOwnership(t *testing.T) {
	router, stores := testRouter()
	ctx := context.Background()

	ownA := certmagic.StorageKeys.CertsSitePrefix(testIssuer, "app.a.example")
	ownB := certmagic.StorageKeys.CertsSitePrefix(testIssuer, "app.b.example")
	// A stale key left behind in fleet b's bucket for a name fleet a owns.
	staleInB := certmagic.StorageKeys.CertsSitePrefix(testIssuer, "app.a.example")

	stores["a"].items[ownA+"/x"] = []byte("v")
	stores["b"].items[ownB+"/x"] = []byte("v")
	stores["b"].items[staleInB+"/y"] = []byte("v")

	keys, err := router.List(ctx, certmagic.StorageKeys.CertsPrefix(testIssuer), false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := strings.Join(keys, " ")
	if !strings.Contains(got, ownA+"/x") {
		t.Errorf("listing = %v, want fleet a's own key", keys)
	}
	if !strings.Contains(got, ownB+"/x") {
		t.Errorf("listing = %v, want fleet b's own key", keys)
	}
	if strings.Contains(got, staleInB+"/y") {
		t.Errorf("listing = %v; a key fleet b holds for a name fleet a owns leaked into the listing", keys)
	}
}

func TestListOfADomainPrefixAsksOnlyItsOwner(t *testing.T) {
	router, stores := testRouter()
	ctx := context.Background()

	prefix := certmagic.StorageKeys.CertsSitePrefix(testIssuer, "app.a.example")
	stores["a"].items[prefix+"/app.a.example.crt"] = []byte("v")
	// The same key in the wrong store must not be seen.
	stores["b"].items[prefix+"/app.a.example.key"] = []byte("v")

	keys, err := router.List(ctx, prefix, true)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 1 || keys[0] != prefix+"/app.a.example.crt" {
		t.Errorf("listing = %v, want only the owning fleet's key", keys)
	}
}

func TestListOfNothingIsNotFound(t *testing.T) {
	router, _ := testRouter()
	if _, err := router.List(context.Background(), "ocsp", false); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("List error = %v, want it to wrap fs.ErrNotExist", err)
	}
}

func TestListOfAnUnownedDomainPrefixIsNotFound(t *testing.T) {
	router, _ := testRouter()
	prefix := certmagic.StorageKeys.CertsSitePrefix(testIssuer, "stranger.example")
	if _, err := router.List(context.Background(), prefix, false); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("List error = %v, want it to wrap fs.ErrNotExist", err)
	}
}

func TestLocksFollowTheirDomain(t *testing.T) {
	router, stores := testRouter()
	ctx := context.Background()

	if err := router.Lock(ctx, "issue_cert_app.a.example"); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if _, held := stores["a"].locks["issue_cert_app.a.example"]; !held {
		t.Error("the issuance lock was not taken in the owning fleet")
	}
	if _, held := stores["b"].locks["issue_cert_app.a.example"]; held {
		t.Error("the issuance lock was taken in another fleet too")
	}
	if err := router.Unlock(ctx, "issue_cert_app.a.example"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if _, held := stores["a"].locks["issue_cert_app.a.example"]; held {
		t.Error("the lock was not released")
	}

	// A wildcard's issuance lock goes to the fleet carrying the wildcard.
	if err := router.Lock(ctx, "issue_cert_*.b.example"); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if _, held := stores["b"].locks["issue_cert_*.b.example"]; !held {
		t.Error("the wildcard's issuance lock was not taken in the owning fleet")
	}

	// Everything else is the ingress's own.
	for _, name := range []string{"storage_clean", "register_acme_account_ops@x.example", "ari_abc"} {
		if err := router.Lock(ctx, name); err != nil {
			t.Fatalf("Lock(%q): %v", name, err)
		}
		if _, held := stores["local"].locks[name]; !held {
			t.Errorf("lock %q was not taken locally", name)
		}
	}
}

func TestUnlockingAnUnownedNameIsNotAnError(t *testing.T) {
	router, _ := testRouter()
	if err := router.Unlock(context.Background(), "issue_cert_stranger.example"); err != nil {
		t.Errorf("Unlock: %v; releasing a lock that was never taken is idempotent", err)
	}
}
