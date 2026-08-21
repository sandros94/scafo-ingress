// Package scafo provides a certmagic.Storage implementation that delegates
// every storage operation to a colocated scafo agent over HTTP.
//
// The agent owns the actual bucket (or whatever backing store it is configured
// with) and is the only party holding bucket credentials. The process using
// this package holds nothing but a loopback endpoint URL and a bearer token, so
// a compromised TLS terminator cannot reach the bucket directly.
//
// The wire contract is the scafo storage protocol version 1; see the README for
// the full endpoint table.
package scafo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/caddyserver/certmagic"
)

// ProtocolVersion is the storage protocol version this client speaks. It is
// sent on every request in the X-Scafo-Storage-Version header so the agent can
// reject clients it cannot serve.
const ProtocolVersion = "1"

// versionHeader carries ProtocolVersion on every request.
const versionHeader = "X-Scafo-Storage-Version"

// DefaultRequestTimeout bounds a single request to the agent. The agent is
// colocated, so anything slower than this is a fault rather than latency.
// It does not bound Lock, which blocks until the lock is acquired or the
// context is done.
const DefaultRequestTimeout = 30 * time.Second

// LockPollInterval is how long Lock waits before retrying after the agent
// reports that the lock is held elsewhere (HTTP 409).
const LockPollInterval = time.Second

// maxErrorBodySize caps how much of an error response body is read before
// giving up on extracting problem details from it.
const maxErrorBodySize = 64 << 10

// Storage implements certmagic.Storage (and therefore certmagic.Locker) by
// forwarding every operation to a scafo agent's HTTP storage API.
//
// A Storage value is safe for concurrent use; it holds no state beyond its
// configuration and an *http.Client.
type Storage struct {
	// base is the agent's base URL with any trailing slash removed, so that
	// request paths can be appended directly.
	base string

	// token is the bearer token presented to the agent.
	token string

	// client performs the HTTP requests. It is dedicated to this Storage so
	// that timeouts and connection pooling are not shared with unrelated
	// traffic.
	client *http.Client
}

// interface guard
var _ certmagic.Storage = (*Storage)(nil)

// NewStorage returns a Storage that talks to the scafo agent at endpoint,
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

// CloseIdleConnections drops connections to the agent that are being kept
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
	drainAndClose(resp)
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
	drainAndClose(resp)
	return nil
}

// Exists reports whether key exists, either as a value or as a prefix of other
// keys ("directory") — certmagic counts both. The agent's stat endpoint
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

// statResponse is the agent's JSON encoding of key metadata. Modified is a
// pointer so that a null (unknown modification time) is distinguishable from
// the zero time.
type statResponse struct {
	Key        string     `json:"key"`
	Modified   *time.Time `json:"modified"`
	Size       int64      `json:"size"`
	IsTerminal bool       `json:"isTerminal"`
}

// Stat returns metadata about key. If the key does not exist the returned
// error wraps fs.ErrNotExist. A null modification time from the agent becomes
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

// Lock acquires the named distributed lock, blocking until the agent grants it
// or ctx is done.
//
// The agent answers 409 while the lock is held elsewhere; Lock then waits
// LockPollInterval and tries again. Any other failure is returned immediately
// rather than retried, so a misconfigured or broken agent surfaces as an error
// instead of an endless poll.
func (s *Storage) Lock(ctx context.Context, name string) error {
	for {
		req, err := s.newRequest(ctx, http.MethodPost, s.lockURL(name), nil)
		if err != nil {
			return err
		}
		resp, err := s.do(req)
		if err == nil {
			drainAndClose(resp)
			return nil
		}
		if statusCode(err) != http.StatusConflict {
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

// Unlock releases the named lock. The agent treats the release as idempotent,
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
	drainAndClose(resp)
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
// owns the body. Any other status becomes an *apiError and the body is closed
// here.
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
	return nil, newAPIError(req, resp)
}

// drainAndClose consumes and closes a response body whose content is not
// needed, so the underlying connection can be reused.
func drainAndClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// problemDetails is the application/problem+json body the agent returns for
// error responses. Only the fields worth surfacing are decoded.
type problemDetails struct {
	Title  string `json:"title"`
	Detail string `json:"detail"`
}

// apiError reports a non-2xx response from the scafo agent.
//
// A 404 unwraps to fs.ErrNotExist, which is what certmagic checks for with
// errors.Is to tell "missing" apart from "broken".
type apiError struct {
	Method string
	Path   string
	Status int
	Title  string
	Detail string
}

// Error implements the error interface, preferring the agent's problem details
// over the bare status text when the agent supplied them.
func (e *apiError) Error() string {
	reason := e.Title
	switch {
	case reason == "":
		reason = e.Detail
	case e.Detail != "":
		reason += ": " + e.Detail
	}
	if reason == "" {
		reason = http.StatusText(e.Status)
	}
	if reason == "" {
		return fmt.Sprintf("scafo storage: %s %s: HTTP %d", e.Method, e.Path, e.Status)
	}
	return fmt.Sprintf("scafo storage: %s %s: HTTP %d: %s", e.Method, e.Path, e.Status, reason)
}

// Unwrap maps a 404 onto fs.ErrNotExist so callers can use errors.Is; other
// statuses have no standard-library counterpart.
func (e *apiError) Unwrap() error {
	if e.Status == http.StatusNotFound {
		return fs.ErrNotExist
	}
	return nil
}

// newAPIError builds an apiError from a failed response, decoding problem
// details when the agent provided them. A body that is missing, oversized or
// malformed is not itself an error: the status code still describes the
// failure.
func newAPIError(req *http.Request, resp *http.Response) *apiError {
	err := &apiError{
		Method: req.Method,
		Path:   req.URL.Path,
		Status: resp.StatusCode,
	}
	if !isProblemJSON(resp.Header.Get("Content-Type")) {
		return err
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySize))
	if readErr != nil {
		return err
	}
	var problem problemDetails
	if json.Unmarshal(body, &problem) == nil {
		err.Title = problem.Title
		err.Detail = problem.Detail
	}
	return err
}

// isProblemJSON reports whether a Content-Type names a JSON problem document.
// Plain application/json is accepted too, since the details are shaped the same
// way and rejecting it would only lose information.
func isProblemJSON(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return mediaType == "application/problem+json" || mediaType == "application/json"
}

// statusCode reports the HTTP status carried by err, or 0 if err did not come
// from an agent response.
func statusCode(err error) int {
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		return apiErr.Status
	}
	return 0
}
