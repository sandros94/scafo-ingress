package storage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"

	"github.com/caddyserver/certmagic"

	"github.com/sandros94/scafo-ingress/fleets"
)

// Router is the certmagic.Storage Caddy is given: one keyspace on the
// outside, one store per fleet plus the ingress's own disk on the inside.
//
// Every key that names a domain is routed to the fleet that owns that domain,
// so a certificate, its private key, its OCSP staple and its issuance lock all
// travel through that fleet's node and that fleet's token and never touch
// another's. Keys that name no domain -- the ACME account above all -- have no
// fleet to belong to and live locally; see keys.go.
type Router struct {
	// owner answers which fleet owns a domain, by name. It is the scafo app's
	// cross-fleet resolution, taken as a function so that the routing rules
	// below can be exercised without one.
	owner func(domain string) (string, bool)

	// order is the fleet names in declaration order, which is the order a
	// listing fans out in.
	order []string

	// stores is one store per fleet, by fleet name.
	stores map[string]certmagic.Storage

	// local holds the keys that name no domain. It is Caddy's own file
	// storage, under its data directory.
	local certmagic.Storage
}

// interface guard
var _ certmagic.Storage = (*Router)(nil)

// NewRouter builds the router for an app's fleets: one HTTP client per fleet,
// and the app's own resolution deciding who owns what.
func NewRouter(app *fleets.App, local certmagic.Storage) *Router {
	order := make([]string, 0, len(app.Fleets()))
	stores := make(map[string]certmagic.Storage, len(app.Fleets()))
	for _, fleet := range app.Fleets() {
		order = append(order, fleet.Name)
		stores[fleet.Name] = NewStorage(fleet.EndpointURL(), fleet.BearerToken(), fleet.StorageRequestTimeout())
	}
	owner := func(domain string) (string, bool) {
		fleet, ok := app.Owner(domain)
		if !ok {
			return "", false
		}
		return fleet.Name, true
	}
	return newRouter(owner, order, stores, local)
}

// newRouter is the constructor the rules are written against.
func newRouter(owner func(string) (string, bool), order []string, stores map[string]certmagic.Storage, local certmagic.Storage) *Router {
	return &Router{owner: owner, order: order, stores: stores, local: local}
}

// CloseIdleConnections drops the connections every fleet's client is keeping
// alive but not using.
func (r *Router) CloseIdleConnections() {
	for _, store := range r.stores {
		if closer, ok := store.(interface{ CloseIdleConnections() }); ok {
			closer.CloseIdleConnections()
		}
	}
}

// storeFor resolves a placement to the one store that may answer for it.
//
// An unowned domain gets no store and no default. Falling back to "the first
// fleet" would put one customer's private key in another customer's bucket,
// which is the one outcome this whole file exists to prevent — so the caller
// reports the key as missing on a read and refuses it on a write.
func (r *Router) storeFor(p placement) (certmagic.Storage, bool) {
	switch p.scope {
	case scopeLocal:
		return r.local, true
	case scopeOwned:
		if p.domain == "" {
			return nil, false
		}
		fleet, ok := r.owner(p.domain)
		if !ok {
			return nil, false
		}
		store, ok := r.stores[fleet]
		return store, ok
	default:
		// A pure prefix holds no value of its own; only a listing is
		// meaningful on one, and List handles that itself. Anything else that
		// reaches here is asking about the shape of the keyspace rather than
		// about a certificate, and the local store is where that shape lives.
		return r.local, true
	}
}

// notOwned is the error a write to an unowned domain gets.
func notOwned(op, key, domain string) error {
	if domain == "" {
		return fmt.Errorf("%s: %s %q: the key names no domain this ingress can place", errPrefix, op, key)
	}
	return fmt.Errorf("%s: %s %q: no fleet serves %q, so there is no bucket to put it in", errPrefix, op, key, domain)
}

// Store puts value at key, in the store that owns it.
func (r *Router) Store(ctx context.Context, key string, value []byte) error {
	p := classify(key)
	store, ok := r.storeFor(p)
	if !ok {
		return notOwned("store", key, p.domain)
	}
	return store.Store(ctx, key, value)
}

// Load retrieves the value at key. A key whose domain no fleet owns is
// reported as missing rather than as broken: CertMagic tells the two apart
// with errors.Is(err, fs.ErrNotExist), and "nobody serves this name" really is
// the absence of a certificate rather than a storage fault.
func (r *Router) Load(ctx context.Context, key string) ([]byte, error) {
	p := classify(key)
	store, ok := r.storeFor(p)
	if !ok {
		return nil, fmt.Errorf("%s: load %q: no fleet serves %q: %w", errPrefix, key, p.domain, fs.ErrNotExist)
	}
	return store.Load(ctx, key)
}

// Delete removes key and everything below it, in the store that owns it.
func (r *Router) Delete(ctx context.Context, key string) error {
	p := classify(key)
	store, ok := r.storeFor(p)
	if !ok {
		return notOwned("delete", key, p.domain)
	}
	return store.Delete(ctx, key)
}

// Exists reports whether key exists in the store that owns it. A key nobody
// owns exists nowhere.
func (r *Router) Exists(ctx context.Context, key string) bool {
	store, ok := r.storeFor(classify(key))
	if !ok {
		return false
	}
	return store.Exists(ctx, key)
}

// Stat returns metadata about key, from the store that owns it.
func (r *Router) Stat(ctx context.Context, key string) (certmagic.KeyInfo, error) {
	p := classify(key)
	store, ok := r.storeFor(p)
	if !ok {
		return certmagic.KeyInfo{}, fmt.Errorf("%s: stat %q: no fleet serves %q: %w", errPrefix, key, p.domain, fs.ErrNotExist)
	}
	return store.Stat(ctx, key)
}

// List returns the keys under prefix.
//
// A prefix that itself names a domain is one fleet's business and is listed
// there. A prefix whose children name domains -- "certificates/<issuer>",
// "ocsp" -- is listed EVERYWHERE and merged, because CertMagic's maintenance
// walks those to find what to renew and what to delete, and a listing that
// showed one fleet's keys would quietly stop maintaining the others'.
//
// The merge filters: a key that names a domain is kept only if it came back
// from the store that owns that domain. That is what keeps a stale key left
// behind in one fleet's bucket from being loaded, renewed or deleted through
// another fleet's node — one fleet's keys never enter another's listing.
func (r *Router) List(ctx context.Context, prefix string, recursive bool) ([]string, error) {
	p := classify(prefix)
	if p.scope != scopeFanout {
		store, ok := r.storeFor(p)
		if !ok {
			return nil, fmt.Errorf("%s: list %q: no fleet serves %q: %w", errPrefix, prefix, p.domain, fs.ErrNotExist)
		}
		return store.List(ctx, prefix, recursive)
	}

	type source struct {
		fleet string // empty for the local disk
		store certmagic.Storage
	}
	sources := []source{{store: r.local}}
	for _, fleet := range r.order {
		if store, ok := r.stores[fleet]; ok {
			sources = append(sources, source{fleet: fleet, store: store})
		}
	}

	found := map[string]struct{}{}
	var anyAnswered bool
	var firstErr error
	for _, src := range sources {
		keys, err := src.store.List(ctx, prefix, recursive)
		if err != nil {
			// A store with nothing under the prefix is not a failure of the
			// listing; a store that is broken is, but only if no other store
			// answered.
			if !errors.Is(err, fs.ErrNotExist) && firstErr == nil {
				firstErr = err
			}
			continue
		}
		anyAnswered = true
		for _, key := range keys {
			kp := classify(key)
			if kp.scope == scopeOwned && !r.belongsTo(kp, src.fleet) {
				continue
			}
			found[key] = struct{}{}
		}
	}
	if !anyAnswered {
		if firstErr != nil {
			return nil, firstErr
		}
		return nil, fmt.Errorf("%s: list %q: %w", errPrefix, prefix, fs.ErrNotExist)
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("%s: list %q: %w", errPrefix, prefix, fs.ErrNotExist)
	}

	keys := make([]string, 0, len(found))
	for key := range found {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, nil
}

// belongsTo reports whether a domain-bearing key returned by the named source
// (empty for the local disk) is one that source is entitled to hold.
func (r *Router) belongsTo(p placement, fleet string) bool {
	if p.domain == "" {
		return false
	}
	owner, ok := r.owner(p.domain)
	if !ok {
		return false
	}
	return owner == fleet
}

// Lock acquires the named lock in the store that owns it, blocking until it is
// granted or ctx is done.
func (r *Router) Lock(ctx context.Context, name string) error {
	p := classifyLock(name)
	store, ok := r.storeFor(p)
	if !ok {
		return notOwned("lock", name, p.domain)
	}
	return store.Lock(ctx, name)
}

// Unlock releases the named lock in the store that owns it.
func (r *Router) Unlock(ctx context.Context, name string) error {
	p := classifyLock(name)
	store, ok := r.storeFor(p)
	if !ok {
		// Nothing was ever locked there, so there is nothing to release and
		// nothing to report: an unlock is idempotent by contract.
		return nil
	}
	return store.Unlock(ctx, name)
}
