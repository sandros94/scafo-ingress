package ingress

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
)

// testDocument is the routing document the lookup tests resolve against:
// application hosts, one of them sharing a name with a platform apex, two
// wildcard routes with exact hosts and a platform apex under them, and a fleet
// API section with the daemon's own denied prefixes.
const testDocument = `{
	"v": 1,
	"routes": [
		{ "host": "api.dev.example", "application": "p",
		  "upstreams": [ { "node": "vps-a", "address": "10.83.0.1:41001" } ] },
		{ "host": "App.Dev.Example", "application": "p",
		  "upstreams": [ { "node": "vps-a", "address": "10.83.0.1:41002" },
		                 { "node": "vps-b", "address": "10.83.0.2:41002" } ] },
		{ "host": "idle.dev.example", "application": "p",
		  "upstreams": [] },
		{ "host": "*.dev.example", "application": "p",
		  "upstreams": [ { "node": "vps-c", "address": "10.83.0.3:41003" } ] },
		{ "host": "*.Shop.Example", "application": "p",
		  "upstreams": [ { "node": "vps-c", "address": "10.83.0.3:41004" } ] },
		{ "host": "pinned.shop.example", "application": "p",
		  "upstreams": [ { "node": "vps-a", "address": "10.83.0.1:41005" } ] }
	],
	"api": {
		"hosts": ["dev.example", "api.dev.example", "ops.dev.example"],
		"upstreams": [ { "node": "vps-a", "address": "127.0.0.1:7400" } ],
		"denied": ["/v1/storage", "/v1/ingress/routes"]
	}
}`

// parseDocument decodes and indexes a document the way Fetch does.
func parseDocument(t *testing.T, raw string) *Document {
	t.Helper()
	var doc Document
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("decoding document: %v", err)
	}
	doc.index()
	return &doc
}

// dials reports the addresses of upstreams, which is all a lookup result is.
func dials(upstreams []*reverseproxy.Upstream) []string {
	addresses := make([]string, 0, len(upstreams))
	for _, upstream := range upstreams {
		addresses = append(addresses, upstream.Dial)
	}
	return addresses
}

func TestLookup(t *testing.T) {
	doc := parseDocument(t, testDocument)

	tests := []struct {
		name        string
		host        string
		path        string
		wantDials   []string
		wantOutcome Outcome
		wantMatched string
	}{
		{
			name:        "route",
			host:        "app.dev.example",
			path:        "/",
			wantDials:   []string{"10.83.0.1:41002", "10.83.0.2:41002"},
			wantOutcome: OutcomeRoute,
			wantMatched: "app.dev.example",
		},
		{
			// The document's own host casing must not decide whether a
			// request matches it.
			name:        "route host is matched case-insensitively",
			host:        NormalizeHost("APP.DEV.EXAMPLE:8443"),
			path:        "/",
			wantDials:   []string{"10.83.0.1:41002", "10.83.0.2:41002"},
			wantOutcome: OutcomeRoute,
			wantMatched: "app.dev.example",
		},
		{
			// A route and an API host share this name; the route wins, and
			// the path it is asked for is irrelevant because denied prefixes
			// only exist on API hosts.
			name:        "a route beats an api host of the same name",
			host:        "api.dev.example",
			path:        "/v1/storage",
			wantDials:   []string{"10.83.0.1:41001"},
			wantOutcome: OutcomeRoute,
			wantMatched: "api.dev.example",
		},
		{
			// The host exists and has nothing running behind it, which is a
			// different answer from "no such host" and must not collapse
			// into it -- nor fall through to the wildcard covering it, which
			// would answer one application's name with another's process.
			name:        "a route with no upstreams still resolves",
			host:        "idle.dev.example",
			path:        "/",
			wantDials:   []string{},
			wantOutcome: OutcomeRoute,
			wantMatched: "idle.dev.example",
		},
		{
			name:        "a wildcard covers one label",
			host:        "one.shop.example",
			path:        "/",
			wantDials:   []string{"10.83.0.3:41004"},
			wantOutcome: OutcomeRoute,
			wantMatched: "*.shop.example",
		},
		{
			// The pattern's own casing is normalized like any other host's,
			// and so is the request's.
			name:        "a wildcard is matched case-insensitively",
			host:        NormalizeHost("ONE.Shop.Example:8443"),
			path:        "/",
			wantDials:   []string{"10.83.0.3:41004"},
			wantOutcome: OutcomeRoute,
			wantMatched: "*.shop.example",
		},
		{
			// One label, and the first one: the lookup is tried once, not
			// walked up the name.
			name:        "a wildcard does not cover two labels",
			host:        "deep.one.shop.example",
			path:        "/",
			wantOutcome: OutcomeUnknown,
		},
		{
			name:        "a wildcard does not cover its own base",
			host:        "shop.example",
			path:        "/",
			wantOutcome: OutcomeUnknown,
		},
		{
			name:        "an exact route beats a wildcard covering it",
			host:        "pinned.shop.example",
			path:        "/",
			wantDials:   []string{"10.83.0.1:41005"},
			wantOutcome: OutcomeRoute,
			wantMatched: "pinned.shop.example",
		},
		{
			// The wildcard *.dev.example covers this name too; the platform
			// apex keeps it, because a wildcard must never be able to take a
			// name away from the fleet API.
			name:        "an api host beats a wildcard covering it",
			host:        "ops.dev.example",
			path:        "/v1/projects",
			wantDials:   []string{"127.0.0.1:7400"},
			wantOutcome: OutcomeRoute,
			wantMatched: "ops.dev.example",
		},
		{
			// And the denied rule of that surface is reached, rather than
			// the request slipping onto the wildcard's upstreams.
			name:        "an api host under a wildcard still denies its own paths",
			host:        "ops.dev.example",
			path:        "/v1/storage/objects/x",
			wantOutcome: OutcomeDenied,
			wantMatched: "ops.dev.example",
		},
		{
			name:        "api host",
			host:        "dev.example",
			path:        "/v1/projects",
			wantDials:   []string{"127.0.0.1:7400"},
			wantOutcome: OutcomeRoute,
			wantMatched: "dev.example",
		},
		{
			name:        "api host, denied prefix exactly",
			host:        "dev.example",
			path:        "/v1/storage",
			wantOutcome: OutcomeDenied,
			wantMatched: "dev.example"},
		{
			name:        "api host, below a denied prefix",
			host:        "dev.example",
			path:        "/v1/storage/objects/certificates/x.crt",
			wantOutcome: OutcomeDenied,
			wantMatched: "dev.example"},
		{
			name:        "api host, denied prefix with a trailing separator",
			host:        "dev.example",
			path:        "/v1/storage/",
			wantOutcome: OutcomeDenied,
			wantMatched: "dev.example"},
		{
			// The boundary is a path segment, not a string prefix.
			name:        "api host, a path that only starts like a denied one",
			host:        "dev.example",
			path:        "/v1/storagex",
			wantDials:   []string{"127.0.0.1:7400"},
			wantOutcome: OutcomeRoute,
			wantMatched: "dev.example"},
		{
			name:        "api host, a sibling of a denied prefix",
			host:        "dev.example",
			path:        "/v1/ingress/ask",
			wantDials:   []string{"127.0.0.1:7400"},
			wantOutcome: OutcomeRoute,
			wantMatched: "dev.example"},
		{
			name:        "api host, a deeper denied prefix",
			host:        "dev.example",
			path:        "/v1/ingress/routes",
			wantOutcome: OutcomeDenied,
			wantMatched: "dev.example"},
		{
			// The daemon may or may not collapse dot segments before it
			// routes; a path that reaches a denied prefix under either
			// reading is refused.
			name:        "api host, a denied prefix reached through dot segments",
			host:        "dev.example",
			path:        "/public/../v1/storage/objects/x",
			wantOutcome: OutcomeDenied,
			wantMatched: "dev.example"},
		{
			name:        "unknown host",
			host:        "nobody.example",
			path:        "/",
			wantOutcome: OutcomeUnknown,
		},
		{
			name:        "empty host",
			host:        "",
			path:        "/",
			wantOutcome: OutcomeUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstreams, outcome, matched := doc.Lookup(tt.host, tt.path)
			if outcome != tt.wantOutcome {
				t.Errorf("outcome = %q, want %q", outcome, tt.wantOutcome)
			}
			got := dials(upstreams)
			if strings.Join(got, ",") != strings.Join(tt.wantDials, ",") {
				t.Errorf("dials = %v, want %v", got, tt.wantDials)
			}
			if matched != tt.wantMatched {
				t.Errorf("matched key = %q, want %q", matched, tt.wantMatched)
			}
		})
	}
}

func TestLookupWithoutDocument(t *testing.T) {
	// "Nothing has been fetched yet" is not "the fleet does not know this
	// host": an operator turning unknown hosts into 404 must not turn a cold
	// start into one too.
	var doc *Document
	upstreams, outcome, matched := doc.Lookup("app.dev.example", "/")
	if outcome != OutcomeUnavailable {
		t.Errorf("outcome = %q, want %q", outcome, OutcomeUnavailable)
	}
	if len(upstreams) != 0 {
		t.Errorf("upstreams = %v, want none", dials(upstreams))
	}
	if matched != "" {
		t.Errorf("matched key = %q, want none", matched)
	}
}

// TestLookupIgnoresAMalformedWildcard covers the document entries this module
// cannot read as a pattern. A wildcard is exactly one leading "*." label and no
// other asterisk; anything else is dropped the way every other unusable entry
// is, rather than being indexed as a name of its own or guessed at.
func TestLookupNeverMatchesARequestCarryingAnAsterisk(t *testing.T) {
	doc := parseDocument(t, `{"v":1,"routes":[
		{"host":"*.shop.example","upstreams":[{"node":"vps-a","address":"10.83.0.1:1"}]}
	]}`)
	for _, host := range []string{"*.shop.example", "a*.shop.example", ".shop.example"} {
		if _, outcome, _ := doc.Lookup(host, "/"); outcome != OutcomeUnknown {
			t.Errorf("Lookup(%q) = %v, want unknown", host, outcome)
		}
	}
}

func TestLookupIgnoresAMalformedWildcard(t *testing.T) {
	doc := parseDocument(t, `{"v":1,"routes":[
		{"host":"*","upstreams":[{"node":"vps-a","address":"10.83.0.1:1"}]},
		{"host":"*.","upstreams":[{"node":"vps-a","address":"10.83.0.1:2"}]},
		{"host":"*.*.dev.example","upstreams":[{"node":"vps-a","address":"10.83.0.1:3"}]},
		{"host":"a.*.dev.example","upstreams":[{"node":"vps-a","address":"10.83.0.1:4"}]},
		{"host":"**.dev.example","upstreams":[{"node":"vps-a","address":"10.83.0.1:5"}]},
		{"host":"*x.dev.example","upstreams":[{"node":"vps-a","address":"10.83.0.1:6"}]},
		{"host":"ok.dev.example","upstreams":[{"node":"vps-a","address":"10.83.0.1:7"}]}
	]}`)

	for _, host := range []string{
		// Nothing above covers a name...
		"one.dev.example",
		"a.b.dev.example",
		"dev.example",
		// ...and none of the malformed patterns is a host either.
		"*.dev.example",
		"a.*.dev.example",
		"*x.dev.example",
	} {
		if _, outcome, matched := doc.Lookup(host, "/"); outcome != OutcomeUnknown {
			t.Errorf("Lookup(%q) = %q via %q, want %q", host, outcome, matched, OutcomeUnknown)
		}
	}

	// The well-formed entry beside them is unaffected.
	if _, outcome, _ := doc.Lookup("ok.dev.example", "/"); outcome != OutcomeRoute {
		t.Errorf("outcome = %q, want %q", outcome, OutcomeRoute)
	}
}

// TestLookupRepeatedWildcardKeepsTheFirst mirrors the rule for repeated names:
// the document holds each key once, and a daemon that repeats one anyway must
// not make the answer depend on how far the scan got.
func TestLookupRepeatedWildcardKeepsTheFirst(t *testing.T) {
	doc := parseDocument(t, `{"v":1,"routes":[
		{"host":"*.dev.example","upstreams":[{"node":"vps-a","address":"10.83.0.1:41001"}]},
		{"host":"*.DEV.example","upstreams":[{"node":"vps-b","address":"10.83.0.2:41001"}]}
	]}`)

	upstreams, outcome, matched := doc.Lookup("one.dev.example", "/")
	if outcome != OutcomeRoute || matched != "*.dev.example" {
		t.Fatalf("outcome = %q via %q, want %q via %q", outcome, matched, OutcomeRoute, "*.dev.example")
	}
	if got := dials(upstreams); strings.Join(got, ",") != "10.83.0.1:41001" {
		t.Errorf("dials = %v, want the first entry's", got)
	}
}

func TestLookupWithoutAPISection(t *testing.T) {
	// A node that does not publish the fleet API sends no api member at all,
	// and every host it does not route is simply unknown.
	doc := parseDocument(t, `{"v":1,"routes":[{"host":"app.dev.example","application":"p",
		"upstreams":[{"node":"vps-a","address":"10.83.0.1:41002"}]}]}`)

	if _, outcome, _ := doc.Lookup("dev.example", "/v1/storage"); outcome != OutcomeUnknown {
		t.Errorf("outcome = %q, want %q", outcome, OutcomeUnknown)
	}
	if _, outcome, _ := doc.Lookup("app.dev.example", "/v1/storage"); outcome != OutcomeRoute {
		t.Errorf("outcome = %q, want %q", outcome, OutcomeRoute)
	}
}

func TestNormalizeHost(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"app.dev.example", "app.dev.example"},
		{"app.dev.example:8443", "app.dev.example"},
		{"App.Dev.Example:443", "app.dev.example"},
		{"  app.dev.example  ", "app.dev.example"},
		{"app.dev.example:", "app.dev.example"},
		{"[2001:db8::1]:443", "2001:db8::1"},
		{"127.0.0.1:80", "127.0.0.1"},
		// Not parseable as host:port, so it is left whole rather than cut at
		// the first colon.
		{"a:b:c", "a:b:c"},
		{"", ""},
	}

	for _, tt := range tests {
		if got := NormalizeHost(tt.in); got != tt.want {
			t.Errorf("NormalizeHost(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

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
	return NewRoutes(endpoint, "test-token", prefer, nil)
}

func TestFetchSendsCredentialsAndPreference(t *testing.T) {
	server := newRoutesServer(t, testDocument)
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
	upstreams, outcome, _ := doc.Lookup("app.dev.example", "/")
	if outcome != OutcomeRoute {
		t.Fatalf("outcome = %q, want %q", outcome, OutcomeRoute)
	}
	if got := dials(upstreams); strings.Join(got, ",") != "10.83.0.1:41002,10.83.0.2:41002" {
		t.Errorf("dials = %v, want the document's order", got)
	}
}

func TestFetchSendsANodeIDVerbatim(t *testing.T) {
	// Past "self" the value is a node id, and the fleet's names are the
	// daemon's to know. This module carries whatever it was given across
	// rather than holding a list of the words it recognizes -- a list that
	// could only ever be wrong about somebody's node.
	server := newRoutesServer(t, testDocument)
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
			server := newRoutesServer(t, testDocument)
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
	server := newRoutesServer(t, testDocument)
	routes := routesFor(t, server, "self")

	routes.Refresh(context.Background())
	if routes.Document() == nil {
		t.Fatal("Document = nil after a successful refresh")
	}

	// Whatever goes wrong next, the routes already known stay known: a daemon
	// restart must not take the ingress down with it.
	server.status.Store(http.StatusServiceUnavailable)
	routes.Refresh(context.Background())
	if _, outcome, _ := routes.Document().Lookup("app.dev.example", "/"); outcome != OutcomeRoute {
		t.Errorf("outcome after a failed refresh = %q, want %q", outcome, OutcomeRoute)
	}

	// And a daemon that comes back replaces it.
	moved := `{"v":1,"routes":[{"host":"app.dev.example","application":"p",
		"upstreams":[{"node":"vps-b","address":"10.83.0.2:41002"}]}]}`
	server.body.Store(&moved)
	server.status.Store(http.StatusOK)
	routes.Refresh(context.Background())
	upstreams, _, _ := routes.Document().Lookup("app.dev.example", "/")
	if got := dials(upstreams); strings.Join(got, ",") != "10.83.0.2:41002" {
		t.Errorf("dials after recovery = %v, want the new document's", got)
	}
}

func TestRefreshWithoutADocumentLeavesNone(t *testing.T) {
	// The cold-start failure: nothing is known, and the lookup says so rather
	// than claiming the host does not exist.
	server := newRoutesServer(t, testDocument)
	server.status.Store(http.StatusServiceUnavailable)
	routes := routesFor(t, server, "self")

	routes.Refresh(context.Background())
	if routes.Document() != nil {
		t.Fatal("Document is set after a refresh that never succeeded")
	}
	if _, outcome, _ := routes.Document().Lookup("app.dev.example", "/"); outcome != OutcomeUnavailable {
		t.Errorf("outcome = %q, want %q", outcome, OutcomeUnavailable)
	}
}

func TestRunPollsUntilTheContextIsDone(t *testing.T) {
	server := newRoutesServer(t, testDocument)
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
	server := newRoutesServer(t, testDocument)
	routes := routesFor(t, server, "self")
	server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	routes.Run(ctx, time.Millisecond)

	if routes.Document() != nil {
		t.Error("Document is set although every fetch failed")
	}
}
