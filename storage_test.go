package scafo

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "s3cr3t-token"

// newTestStorage starts an httptest server running handler and returns a
// Storage pointed at it. The server is shut down when the test ends.
func newTestStorage(t *testing.T, handler http.Handler) *Storage {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parsing test server URL: %v", err)
	}
	return NewStorage(base, testToken)
}

// notFound writes the daemon's 404 response, including a problem document so
// the tests also cover error decoding on the not-found path.
func notFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, `{"title":"not found","detail":"no such key"}`)
}

// countingHandler counts the requests it serves, so tests can assert how many
// round trips an operation made.
type countingHandler struct {
	mu     sync.Mutex
	calls  int
	handle func(w http.ResponseWriter, r *http.Request, call int)
}

func (h *countingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.calls++
	call := h.calls
	h.mu.Unlock()
	h.handle(w, r, call)
}

func (h *countingHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

func TestEveryRequestCarriesAuthAndVersionHeaders(t *testing.T) {
	type record struct {
		method  string
		path    string
		auth    string
		version string
	}
	var (
		mu   sync.Mutex
		seen []record
	)

	storage := newTestStorage(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, record{
			method:  r.Method,
			path:    r.URL.Path,
			auth:    r.Header.Get("Authorization"),
			version: r.Header.Get(versionHeader),
		})
		mu.Unlock()

		switch {
		case r.URL.Path == "/v1/storage/list":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[]`)
		case r.URL.Path == "/v1/storage/stat":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"key":"a","modified":null,"size":0,"isTerminal":true}`)
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, "value")
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))

	ctx := context.Background()
	if err := storage.Store(ctx, "a", []byte("v")); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if _, err := storage.Load(ctx, "a"); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := storage.Delete(ctx, "a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !storage.Exists(ctx, "a") {
		t.Fatal("Exists = false, want true")
	}
	if _, err := storage.List(ctx, "a", true); err != nil {
		t.Fatalf("List: %v", err)
	}
	if _, err := storage.Stat(ctx, "a"); err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if err := storage.Lock(ctx, "lk"); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if err := storage.Unlock(ctx, "lk"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	// Store, Load, Delete, Exists(stat), List, Stat, Lock, Unlock.
	if len(seen) != 8 {
		t.Fatalf("got %d requests, want 8: %+v", len(seen), seen)
	}
	for _, rec := range seen {
		if rec.auth != "Bearer "+testToken {
			t.Errorf("%s %s: Authorization = %q, want %q", rec.method, rec.path, rec.auth, "Bearer "+testToken)
		}
		if rec.version != ProtocolVersion {
			t.Errorf("%s %s: %s = %q, want %q", rec.method, rec.path, versionHeader, rec.version, ProtocolVersion)
		}
	}

	wantMethods := []string{
		http.MethodPut, http.MethodGet, http.MethodDelete, http.MethodGet,
		http.MethodGet, http.MethodGet, http.MethodPost, http.MethodDelete,
	}
	for i, want := range wantMethods {
		if seen[i].method != want {
			t.Errorf("request %d: method = %s, want %s", i, seen[i].method, want)
		}
	}
}

func TestObjectKeysMapOntoPathSegments(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want string
	}{
		{
			name: "certmagic key keeps its separators",
			key:  "certificates/acme/example.com/example.com.crt",
			want: "/v1/storage/objects/certificates/acme/example.com/example.com.crt",
		},
		{
			name: "unsafe characters are escaped per segment",
			key:  "odd keys/a?b#c/d",
			want: "/v1/storage/objects/odd%20keys/a%3Fb%23c/d",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got string
			storage := newTestStorage(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.URL.EscapedPath()
				w.WriteHeader(http.StatusNoContent)
			}))
			if err := storage.Store(context.Background(), tt.key, []byte("v")); err != nil {
				t.Fatalf("Store: %v", err)
			}
			if got != tt.want {
				t.Errorf("request path = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStorePutsRawBody(t *testing.T) {
	var body []byte
	storage := newTestStorage(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	want := []byte("\x00raw bytes\xff")
	if err := storage.Store(context.Background(), "k", want); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if string(body) != string(want) {
		t.Errorf("body = %q, want %q", body, want)
	}
}

func TestLoadReturnsRawBytes(t *testing.T) {
	storage := newTestStorage(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("certificate-pem"))
	}))
	got, err := storage.Load(context.Background(), "k")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(got) != "certificate-pem" {
		t.Errorf("Load = %q, want %q", got, "certificate-pem")
	}
}

func TestNotFoundIsErrNotExist(t *testing.T) {
	storage := newTestStorage(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		notFound(w)
	}))
	ctx := context.Background()

	t.Run("Load", func(t *testing.T) {
		_, err := storage.Load(ctx, "missing")
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("Load error = %v, want fs.ErrNotExist", err)
		}
	})
	t.Run("Delete", func(t *testing.T) {
		err := storage.Delete(ctx, "missing")
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("Delete error = %v, want fs.ErrNotExist", err)
		}
	})
	t.Run("Stat", func(t *testing.T) {
		_, err := storage.Stat(ctx, "missing")
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("Stat error = %v, want fs.ErrNotExist", err)
		}
	})
	t.Run("List", func(t *testing.T) {
		_, err := storage.List(ctx, "missing", false)
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("List error = %v, want fs.ErrNotExist", err)
		}
	})
}

func TestExists(t *testing.T) {
	tests := []struct {
		name    string
		respond func(w http.ResponseWriter)
		want    bool
	}{
		{
			name: "terminal key exists",
			respond: func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"key":"k","modified":null,"size":3,"isTerminal":true}`)
			},
			want: true,
		},
		{
			name: "directory prefix counts as existing",
			respond: func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"key":"k","modified":null,"size":0,"isTerminal":false}`)
			},
			want: true,
		},
		{
			name:    "missing key",
			respond: notFound,
			want:    false,
		},
		{
			name: "node failure is not existence",
			respond: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			storage := newTestStorage(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tt.respond(w)
			}))
			if got := storage.Exists(context.Background(), "k"); got != tt.want {
				t.Errorf("Exists = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestListDecodesKeysAndSendsQuery(t *testing.T) {
	var query url.Values
	storage := newTestStorage(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `["certificates/acme/a.example","certificates/acme/b.example"]`)
	}))

	got, err := storage.List(context.Background(), "certificates/acme", true)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"certificates/acme/a.example", "certificates/acme/b.example"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("List = %v, want %v", got, want)
	}
	if got := query.Get("prefix"); got != "certificates/acme" {
		t.Errorf("prefix = %q, want %q", got, "certificates/acme")
	}
	if got := query.Get("recursive"); got != "true" {
		t.Errorf("recursive = %q, want %q", got, "true")
	}
}

func TestListSendsRecursiveFalse(t *testing.T) {
	var recursive string
	storage := newTestStorage(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recursive = r.URL.Query().Get("recursive")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[]`)
	}))
	if _, err := storage.List(context.Background(), "p", false); err != nil {
		t.Fatalf("List: %v", err)
	}
	if recursive != "false" {
		t.Errorf("recursive = %q, want %q", recursive, "false")
	}
}

func TestStatDecodesKeyInfo(t *testing.T) {
	var gotKey string
	storage := newTestStorage(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.URL.Query().Get("key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"key":"certificates/acme/x.crt","modified":"2026-04-01T12:30:00Z","size":4096,"isTerminal":true}`)
	}))

	info, err := storage.Stat(context.Background(), "certificates/acme/x.crt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if gotKey != "certificates/acme/x.crt" {
		t.Errorf("key query = %q, want %q", gotKey, "certificates/acme/x.crt")
	}
	if info.Key != "certificates/acme/x.crt" {
		t.Errorf("Key = %q, want %q", info.Key, "certificates/acme/x.crt")
	}
	if info.Size != 4096 {
		t.Errorf("Size = %d, want 4096", info.Size)
	}
	if !info.IsTerminal {
		t.Error("IsTerminal = false, want true")
	}
	want := time.Date(2026, 4, 1, 12, 30, 0, 0, time.UTC)
	if !info.Modified.Equal(want) {
		t.Errorf("Modified = %v, want %v", info.Modified, want)
	}
}

func TestStatNullModifiedIsZeroTime(t *testing.T) {
	storage := newTestStorage(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"key":"k","modified":null,"size":7,"isTerminal":true}`)
	}))
	info, err := storage.Stat(context.Background(), "k")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !info.Modified.IsZero() {
		t.Errorf("Modified = %v, want zero time", info.Modified)
	}
	if info.Size != 7 {
		t.Errorf("Size = %d, want 7", info.Size)
	}
}

func TestLockAcquiredImmediately(t *testing.T) {
	handler := &countingHandler{handle: func(w http.ResponseWriter, r *http.Request, call int) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/storage/locks/issue_cert_example.com" {
			t.Errorf("path = %q, want the lock path", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}}
	storage := newTestStorage(t, handler)

	if err := storage.Lock(context.Background(), "issue_cert_example.com"); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if got := handler.count(); got != 1 {
		t.Errorf("requests = %d, want 1", got)
	}
}

func TestLockPollsAfterConflict(t *testing.T) {
	handler := &countingHandler{handle: func(w http.ResponseWriter, r *http.Request, call int) {
		if call == 1 {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"title":"lock held","detail":"held by another process"}`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}}
	storage := newTestStorage(t, handler)

	start := time.Now()
	if err := storage.Lock(context.Background(), "lk"); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	elapsed := time.Since(start)

	if got := handler.count(); got != 2 {
		t.Fatalf("requests = %d, want 2 (one conflict, one success)", got)
	}
	// The retry must be spaced by the poll interval, not spun on.
	if elapsed < LockPollInterval {
		t.Errorf("elapsed = %v, want at least the %v poll interval", elapsed, LockPollInterval)
	}
}

func TestLockReturnsPromptlyOnContextCancel(t *testing.T) {
	handler := &countingHandler{handle: func(w http.ResponseWriter, r *http.Request, call int) {
		w.WriteHeader(http.StatusConflict)
	}}
	storage := newTestStorage(t, handler)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)

	start := time.Now()
	err := storage.Lock(ctx, "lk")
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Lock error = %v, want context.Canceled", err)
	}
	// Cancellation must interrupt the wait rather than being noticed only
	// after the next poll.
	if elapsed >= LockPollInterval {
		t.Errorf("elapsed = %v, want well under the %v poll interval", elapsed, LockPollInterval)
	}
}

func TestLockFailsFastOnOtherErrors(t *testing.T) {
	handler := &countingHandler{handle: func(w http.ResponseWriter, r *http.Request, call int) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"title":"backend unavailable"}`)
	}}
	storage := newTestStorage(t, handler)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := storage.Lock(ctx, "lk")
	if err == nil {
		t.Fatal("Lock error = nil, want an error")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Lock spun until the deadline instead of failing: %v", err)
	}
	if got := handler.count(); got != 1 {
		t.Errorf("requests = %d, want 1 (no retry on non-conflict errors)", got)
	}
	if !strings.Contains(err.Error(), "backend unavailable") {
		t.Errorf("error = %q, want it to mention the problem title", err)
	}
}

func TestUnlockIssuesOneDelete(t *testing.T) {
	handler := &countingHandler{handle: func(w http.ResponseWriter, r *http.Request, call int) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		if r.URL.Path != "/v1/storage/locks/lk" {
			t.Errorf("path = %q, want /v1/storage/locks/lk", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}}
	storage := newTestStorage(t, handler)

	if err := storage.Unlock(context.Background(), "lk"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if got := handler.count(); got != 1 {
		t.Errorf("requests = %d, want 1", got)
	}
}

func TestProblemJSONIsSurfacedInErrors(t *testing.T) {
	storage := newTestStorage(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"type":"about:blank","title":"bucket unreachable","detail":"dial tcp: connection refused","status":502}`)
	}))

	err := storage.Store(context.Background(), "k", []byte("v"))
	if err == nil {
		t.Fatal("Store error = nil, want an error")
	}
	for _, want := range []string{"bucket unreachable", "dial tcp: connection refused", "502"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
}

func TestNonProblemErrorFallsBackToStatus(t *testing.T) {
	storage := newTestStorage(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "nope")
	}))

	err := storage.Store(context.Background(), "k", []byte("v"))
	if err == nil {
		t.Fatal("Store error = nil, want an error")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "Forbidden") {
		t.Errorf("error = %q, want it to report the status", err)
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Error("a 403 must not be reported as fs.ErrNotExist")
	}
}

func TestEndpointPathPrefixIsPreserved(t *testing.T) {
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	base, err := url.Parse(server.URL + "/scafo/")
	if err != nil {
		t.Fatalf("parsing base URL: %v", err)
	}
	storage := NewStorage(base, testToken)

	if err := storage.Store(context.Background(), "k", []byte("v")); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if path != "/scafo/v1/storage/objects/k" {
		t.Errorf("path = %q, want %q", path, "/scafo/v1/storage/objects/k")
	}
}

func TestNewStorageUsesBoundedTimeout(t *testing.T) {
	storage := NewStorage(&url.URL{Scheme: "http", Host: "127.0.0.1:1"}, testToken)
	if storage.client.Timeout != DefaultRequestTimeout {
		t.Errorf("client timeout = %v, want %v", storage.client.Timeout, DefaultRequestTimeout)
	}
}
