// Package storage provides a certmagic.Storage implementation that delegates
// every storage operation to a colocated scafo node daemon over HTTP.
//
// The daemon owns the actual bucket (or whatever backing store it is configured
// with) and is the only party holding bucket credentials. The process using
// this package holds nothing but a loopback endpoint URL and a bearer token, so
// a compromised TLS terminator cannot reach the bucket directly.
//
// The wire contract is the scafo storage protocol version 1; see PROTOCOL.md
// for the full endpoint table.
package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/caddyserver/certmagic"

	"github.com/sandros94/scafo-ingress/internal/nodeapi"
)

// errPrefix is the name this module gives itself in the errors it returns. It
// is passed to the shared helpers in internal/nodeapi, which build errors on
// this module's behalf and must attribute them to it rather than to whichever
// scafo module happens to share the daemon.
const errPrefix = "scafo storage"

// ProtocolVersion is the storage protocol version this client speaks. It is
// sent on every request in the X-Scafo-Storage-Version header so the daemon
// can reject clients it cannot serve.
const ProtocolVersion = "1"

// versionHeader carries ProtocolVersion on every request.
const versionHeader = "X-Scafo-Storage-Version"

// DefaultRequestTimeout bounds a single request to the daemon. The daemon is
// colocated, so anything slower than this is a fault rather than latency.
// It does not bound Lock, which blocks until the lock is acquired or the
// context is done.
const DefaultRequestTimeout = 30 * time.Second

// LockPollInterval is how long Lock waits before retrying after the daemon
// reports that the lock is held elsewhere (HTTP 409).
const LockPollInterval = time.Second

// Storage implements certmagic.Storage (and therefore certmagic.Locker) by
// forwarding every operation to a scafo node daemon's HTTP storage API.
//
// A Storage value is safe for concurrent use; it holds no state beyond its
// configuration and an *http.Client.
type Storage struct {
	// base is the daemon's base URL with any trailing slash removed, so that
	// request paths can be appended directly.
	base string

	// token is the bearer token presented to the daemon.
	token string

	// client performs the HTTP requests. It is dedicated to this Storage so
	// that timeouts and connection pooling are not shared with unrelated
	// traffic.
	client *http.Client
}

// interface guard
var _ certmagic.Storage = (*Storage)(nil)

// NewStorage returns a Storage that talks to the scafo node daemon at endpoint,
// authenticating with the given bearer token.
//
// endpoint may include a path prefix (e.g. https://host/scafo); protocol paths
// are appended to it. A nil endpoint or an empty token is a programming error
// and yields a Storage that fails every request.
func NewStorage(endpoint *url.URL, token string) *Storage {
	var base string
	if endpoint != nil {
		base = strings.TrimSuffix(endpoint.String(), "/")
	}
	return &Storage{
		base:  base,
		token: token,
		client: &http.Client{
			Timeout: DefaultRequestTimeout,
		},
	}
}

// CloseIdleConnections drops connections to the daemon that are being kept
// alive but are not in use. It is meant for teardown; the Storage remains
// usable afterwards.
func (s *Storage) CloseIdleConnections() {
	if s.client != nil {
		s.client.CloseIdleConnections()
	}
}

// Store puts value at key, creating or overwriting it.
func (s *Storage) Store(ctx context.Context, key string, value []byte) error {
	req, err := s.newRequest(ctx, http.MethodPut, s.objectURL(key), bytes.NewReader(value))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := s.do(req)
	if err != nil {
		return err
	}
	nodeapi.DrainAndClose(resp)
	return nil
}

// Load retrieves the value at key. If the key does not exist the returned
// error wraps fs.ErrNotExist, as certmagic requires.
func (s *Storage) Load(ctx context.Context, key string) ([]byte, error) {
	req, err := s.newRequest(ctx, http.MethodGet, s.objectURL(key), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	value, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("scafo storage: reading value for %q: %w", key, err)
	}
	return value, nil
}

// Delete removes key and, if key is a prefix of other keys, everything below
// it. If nothing existed at key the returned error wraps fs.ErrNotExist.
func (s *Storage) Delete(ctx context.Context, key string) error {
	req, err := s.newRequest(ctx, http.MethodDelete, s.objectURL(key), nil)
	if err != nil {
		return err
	}
	resp, err := s.do(req)
	if err != nil {
		return err
	}
	nodeapi.DrainAndClose(resp)
	return nil
}

// Exists reports whether key exists, either as a value or as a prefix of other
// keys ("directory") — certmagic counts both. The daemon's stat endpoint
// answers 200 for both shapes and 404 for neither, so existence is exactly a
// successful Stat.
func (s *Storage) Exists(ctx context.Context, key string) bool {
	_, err := s.Stat(ctx, key)
	return err == nil
}

// List returns the keys under prefix. When recursive is false only the
// immediate children are returned; when it is true the whole subtree is walked.
func (s *Storage) List(ctx context.Context, prefix string, recursive bool) ([]string, error) {
	query := url.Values{
		"prefix":    []string{prefix},
		"recursive": []string{strconv.FormatBool(recursive)},
	}
	req, err := s.newRequest(ctx, http.MethodGet, s.base+"/v1/storage/list?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var keys []string
	if err := json.NewDecoder(resp.Body).Decode(&keys); err != nil {
		return nil, fmt.Errorf("scafo storage: decoding key list for %q: %w", prefix, err)
	}
	return keys, nil
}

// statResponse is the daemon's JSON encoding of key metadata. Modified is a
// pointer so that a null (unknown modification time) is distinguishable from
// the zero time.
type statResponse struct {
	Key        string     `json:"key"`
	Modified   *time.Time `json:"modified"`
	Size       int64      `json:"size"`
	IsTerminal bool       `json:"isTerminal"`
}

// Stat returns metadata about key. If the key does not exist the returned
// error wraps fs.ErrNotExist. A null modification time from the daemon becomes
// the zero time.Time, which certmagic treats as "unknown".
func (s *Storage) Stat(ctx context.Context, key string) (certmagic.KeyInfo, error) {
	query := url.Values{"key": []string{key}}
	req, err := s.newRequest(ctx, http.MethodGet, s.base+"/v1/storage/stat?"+query.Encode(), nil)
	if err != nil {
		return certmagic.KeyInfo{}, err
	}
	resp, err := s.do(req)
	if err != nil {
		return certmagic.KeyInfo{}, err
	}
	defer resp.Body.Close()
	var stat statResponse
	if err := json.NewDecoder(resp.Body).Decode(&stat); err != nil {
		return certmagic.KeyInfo{}, fmt.Errorf("scafo storage: decoding stat for %q: %w", key, err)
	}
	info := certmagic.KeyInfo{
		Key:        stat.Key,
		Size:       stat.Size,
		IsTerminal: stat.IsTerminal,
	}
	if stat.Modified != nil {
		info.Modified = *stat.Modified
	}
	return info, nil
}

// Lock acquires the named distributed lock, blocking until the daemon grants
// it or ctx is done.
//
// The daemon answers 409 while the lock is held elsewhere; Lock then waits
// LockPollInterval and tries again. Any other failure is returned immediately
// rather than retried, so a misconfigured or broken daemon surfaces as an error
// instead of an endless poll.
func (s *Storage) Lock(ctx context.Context, name string) error {
	for {
		req, err := s.newRequest(ctx, http.MethodPost, s.lockURL(name), nil)
		if err != nil {
			return err
		}
		resp, err := s.do(req)
		if err == nil {
			nodeapi.DrainAndClose(resp)
			return nil
		}
		if nodeapi.StatusCode(err) != http.StatusConflict {
			return err
		}

		timer := time.NewTimer(LockPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// Unlock releases the named lock. The daemon treats the release as idempotent,
// so it is issued exactly once and a lock that was already gone is not an
// error.
func (s *Storage) Unlock(ctx context.Context, name string) error {
	req, err := s.newRequest(ctx, http.MethodDelete, s.lockURL(name), nil)
	if err != nil {
		return err
	}
	resp, err := s.do(req)
	if err != nil {
		return err
	}
	nodeapi.DrainAndClose(resp)
	return nil
}

// objectURL builds the URL for the object endpoint of key.
func (s *Storage) objectURL(key string) string {
	return s.base + "/v1/storage/objects/" + escapeKeyPath(key)
}

// lockURL builds the URL for the lock endpoint of name. A lock name is a
// single path component, so it is escaped as a whole.
func (s *Storage) lockURL(name string) string {
	return s.base + "/v1/storage/locks/" + url.PathEscape(name)
}

// escapeKeyPath percent-encodes each '/'-separated component of key while
// leaving the separators intact, so the key hierarchy survives into the
// request URI. Certmagic keys are already sanitized, so this normally changes
// nothing; it exists so that unusual keys cannot alter the request path.
func escapeKeyPath(key string) string {
	components := strings.Split(key, "/")
	for i, component := range components {
		components[i] = url.PathEscape(component)
	}
	return strings.Join(components, "/")
}

// newRequest builds a request carrying the credentials and protocol version
// that every scafo storage call must present.
func (s *Storage) newRequest(ctx context.Context, method, rawURL string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, fmt.Errorf("scafo storage: building %s request: %w", method, err)
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set(versionHeader, ProtocolVersion)
	return req, nil
}

// do sends req and returns the response only for 2xx statuses; the caller then
// owns the body. Any other status becomes a *nodeapi.APIError and the body is
// closed here.
func (s *Storage) do(req *http.Request) (*http.Response, error) {
	client := s.client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("scafo storage: %s %s: %w", req.Method, req.URL.Path, err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	return nil, nodeapi.NewAPIError(errPrefix, req, resp)
}
