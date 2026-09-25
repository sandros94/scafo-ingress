package fleets

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// routesServer is a node daemon stand-in that serves a document, records what
// it was asked, and can be told to fail.
type routesServer struct {
	*httptest.Server

	// requests counts served requests, whatever the outcome.
	requests atomic.Int64

	// body is what to answer with while status is 200.
	body atomic.Pointer[string]

	// status is the status to answer with; 200 serves body.
	status atomic.Int64

	// lastAuth, lastPrefer and lastRawQuery record the last request's
	// credentials and query. The raw query is kept as well as the parsed
	// value, because "no prefer parameter" and "an empty one" are the same
	// after parsing and are not the same on the wire.
	lastAuth     atomic.Pointer[string]
	lastPrefer   atomic.Pointer[string]
	lastRawQuery atomic.Pointer[string]
}

func newRoutesServer(t *testing.T, body string) *routesServer {
	t.Helper()
	server := &routesServer{}
	server.body.Store(&body)
	server.status.Store(http.StatusOK)
	server.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		prefer := r.URL.Query().Get("prefer")
		rawQuery := r.URL.RawQuery
		server.lastAuth.Store(&auth)
		server.lastPrefer.Store(&prefer)
		server.lastRawQuery.Store(&rawQuery)
		server.requests.Add(1)

		if r.URL.Path != "/v1/ingress/routes" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if status := int(server.status.Load()); status != http.StatusOK {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"title":"unavailable","detail":"the node is not ready"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(*server.body.Load()))
	}))
	t.Cleanup(server.Close)
	return server
}

// routesFor returns a Routes pointed at server.
func routesFor(t *testing.T, server *routesServer, prefer string) *Routes {
	t.Helper()
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parsing test server URL: %v", err)
	}
	return NewRoutes(endpoint, "test-token", prefer, 0, nil)
}

// routeDials reports the addresses a document holds for an exact host.
func routeDials(doc *Document, host string) string {
	upstreams, _ := doc.RouteUpstreams(host)
	return strings.Join(dials(upstreams), ",")
}

func TestFetchSendsCredentialsAndPreference(t *testing.T) {
	server := newRoutesServer(t, oneFleetDocument)
	routes := routesFor(t, server, "self")

	doc, err := routes.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := *server.lastAuth.Load(); got != "Bearer test-token" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer test-token")
	}
	if got := *server.lastPrefer.Load(); got != "self" {
		t.Errorf("prefer = %q, want %q", got, "self")
	}
	if got := routeDials(doc, "app.dev.example"); got != "10.83.0.1:41002,10.83.0.2:41002" {
		t.Errorf("dials = %v, want the document's order", got)
	}
}

func TestFetchSendsANodeIDVerbatim(t *testing.T) {
	// Past "self" the value is a node id, and the fleet's names are the
	// daemon's to know. This module carries whatever it was given across
	// rather than holding a list of the words it recognizes -- a list that
	// could only ever be wrong about somebody's node.
	server := newRoutesServer(t, oneFleetDocument)
	routes := routesFor(t, server, "vps-b")

	if _, err := routes.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := *server.lastPrefer.Load(); got != "vps-b" {
		t.Errorf("prefer = %q, want %q", got, "vps-b")
	}
}

func TestFetchOmitsThePreferenceEntirely(t *testing.T) {
	// Asking for the fleet's own order means sending no parameter: the daemon
	// reads any value it does not recognize as a node id, so a "none" on the
	// wire would name no node and quietly do nothing at all.
	for _, prefer := range []string{PreferNone, ""} {
		t.Run("prefer="+prefer, func(t *testing.T) {
			server := newRoutesServer(t, oneFleetDocument)
			routes := routesFor(t, server, prefer)

			if got := routes.requestURL(); strings.Contains(got, "prefer") {
				t.Errorf("requestURL = %q, want no prefer parameter at all", got)
			}
			if _, err := routes.Fetch(context.Background()); err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if got := *server.lastRawQuery.Load(); got != "" {
				t.Errorf("query = %q, want it empty", got)
			}
		})
	}
}

func TestFetchErrors(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		status  int
		wantErr string
	}{
		{
			name:    "problem details from the daemon",
			status:  http.StatusServiceUnavailable,
			wantErr: "unavailable: the node is not ready",
		},
		{
			name:    "unparseable body",
			body:    `{"v":1,"routes":`,
			status:  http.StatusOK,
			wantErr: "decoding routing document",
		},
		{
			name:    "a document version this build cannot read",
			body:    `{"v":2,"routes":[]}`,
			status:  http.StatusOK,
			wantErr: "routing document version 2, want 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newRoutesServer(t, tt.body)
			server.status.Store(int64(tt.status))
			routes := routesFor(t, server, "self")

			_, err := routes.Fetch(context.Background())
			if err == nil {
				t.Fatalf("Fetch error = nil, want one mentioning %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Fetch error = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestRefreshKeepsTheLastGoodDocument(t *testing.T) {
	server := newRoutesServer(t, oneFleetDocument)
	routes := routesFor(t, server, "self")

	routes.Refresh(context.Background())
	if routes.Document() == nil {
		t.Fatal("Document = nil after a successful refresh")
	}

	// Whatever goes wrong next, the routes already known stay known: a daemon
	// restart must not take the ingress down with it.
	server.status.Store(http.StatusServiceUnavailable)
	routes.Refresh(context.Background())
	if _, ok := routes.Document().RouteUpstreams("app.dev.example"); !ok {
		t.Error("the route is gone after a failed refresh; the last document must keep answering")
	}

	// And a daemon that comes back replaces it.
	moved := `{"v":1,"routes":[{"host":"app.dev.example","application":"p",
		"upstreams":[{"node":"vps-b","address":"10.83.0.2:41002"}]}]}`
	server.body.Store(&moved)
	server.status.Store(http.StatusOK)
	routes.Refresh(context.Background())
	if got := routeDials(routes.Document(), "app.dev.example"); got != "10.83.0.2:41002" {
		t.Errorf("dials after recovery = %v, want the new document's", got)
	}
}

func TestRefreshWithoutADocumentLeavesNone(t *testing.T) {
	// The cold-start failure: nothing is known, and the app says so rather
	// than claiming the host does not exist.
	server := newRoutesServer(t, oneFleetDocument)
	server.status.Store(http.StatusServiceUnavailable)
	routes := routesFor(t, server, "self")

	routes.Refresh(context.Background())
	if routes.Document() != nil {
		t.Fatal("Document is set after a refresh that never succeeded")
	}
	if routes.Generation() != 0 {
		t.Errorf("generation = %d after no successful fetch, want 0", routes.Generation())
	}
}

func TestAnUnchangedDocumentDoesNotCountAsAChange(t *testing.T) {
	// The generation is what makes the cross-fleet index rebuild and a host
	// conflict be logged. A daemon that answers the same thing every five
	// seconds must not make either of those happen every five seconds.
	server := newRoutesServer(t, oneFleetDocument)
	routes := routesFor(t, server, "self")

	routes.Refresh(context.Background())
	first := routes.Generation()
	if first != 1 {
		t.Fatalf("generation = %d after the first document, want 1", first)
	}
	before := routes.Document()

	routes.Refresh(context.Background())
	if got := routes.Generation(); got != first {
		t.Errorf("generation = %d after an identical document, want %d", got, first)
	}
	if routes.Document() != before {
		t.Error("the document pointer was replaced although nothing changed")
	}

	changed := `{"v":1,"routes":[{"host":"other.dev.example","upstreams":[]}]}`
	server.body.Store(&changed)
	routes.Refresh(context.Background())
	if got := routes.Generation(); got != first+1 {
		t.Errorf("generation = %d after a different document, want %d", got, first+1)
	}
}

func TestRefreshNotifiesOnChangeOnlyWhenSomethingChanged(t *testing.T) {
	server := newRoutesServer(t, oneFleetDocument)
	routes := routesFor(t, server, "self")
	var changes int
	routes.OnChange(func() { changes++ })

	routes.Refresh(context.Background())
	routes.Refresh(context.Background())
	if changes != 1 {
		t.Errorf("OnChange fired %d times for one document, want 1", changes)
	}
}

func TestRunPollsUntilTheContextIsDone(t *testing.T) {
	server := newRoutesServer(t, oneFleetDocument)
	routes := routesFor(t, server, "self")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		routes.Run(ctx, time.Millisecond)
	}()

	// Wait for the poller to have fetched at least twice, so that both the
	// first tick and the loop around it are covered.
	deadline := time.Now().Add(5 * time.Second)
	for server.requests.Load() < 2 {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("the poller made %d requests in 5s, want at least 2", server.requests.Load())
		}
		time.Sleep(time.Millisecond)
	}
	if routes.Document() == nil {
		t.Error("Document = nil while the poller is running")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}

	// Nothing is fetched once the poller has returned; a reload must not leave
	// a goroutine talking to the daemon on the old configuration's behalf.
	settled := server.requests.Load()
	time.Sleep(20 * time.Millisecond)
	if got := server.requests.Load(); got != settled {
		t.Errorf("the poller made %d more requests after returning", got-settled)
	}
}

func TestRunSurvivesAnUnreachableDaemon(t *testing.T) {
	server := newRoutesServer(t, oneFleetDocument)
	routes := routesFor(t, server, "self")
	server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	routes.Run(ctx, time.Millisecond)

	if routes.Document() != nil {
		t.Error("Document is set although every fetch failed")
	}
}
