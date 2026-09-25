package fleets

import (
	"strings"
	"testing"
)

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

func TestWildcardBase(t *testing.T) {
	tests := []struct {
		host string
		base string
		ok   bool
	}{
		{"*.example.com", "example.com", true},
		{"*.a.example.com", "a.example.com", true},
		{"*", "", false},
		{"*.", "", false},
		{"*.*.example.com", "", false},
		{"a.*.example.com", "", false},
		{"*x.example.com", "", false},
		{"example.com", "", false},
	}
	for _, tt := range tests {
		base, ok := WildcardBase(tt.host)
		if ok != tt.ok || base != tt.base {
			t.Errorf("WildcardBase(%q) = %q, %v; want %q, %v", tt.host, base, ok, tt.base, tt.ok)
		}
	}
}

func TestWildcardCover(t *testing.T) {
	tests := []struct {
		host string
		base string
		ok   bool
	}{
		{"a.example.com", "example.com", true},
		{"a.b.example.com", "b.example.com", true},
		{"example", "", false},
		{"", "", false},
		{".example.com", "", false},
		{"*.example.com", "", false},
	}
	for _, tt := range tests {
		base, ok := WildcardCover(tt.host)
		if ok != tt.ok || base != tt.base {
			t.Errorf("WildcardCover(%q) = %q, %v; want %q, %v", tt.host, base, ok, tt.base, tt.ok)
		}
	}
}

func TestIndexIgnoresAMalformedWildcard(t *testing.T) {
	app := testApp(t, testFleet{name: "one", document: `{"v":1,"routes":[
		{"host":"*","upstreams":[{"node":"vps-a","address":"10.83.0.1:1"}]},
		{"host":"*.","upstreams":[{"node":"vps-a","address":"10.83.0.1:2"}]},
		{"host":"*.*.dev.example","upstreams":[{"node":"vps-a","address":"10.83.0.1:3"}]},
		{"host":"a.*.dev.example","upstreams":[{"node":"vps-a","address":"10.83.0.1:4"}]},
		{"host":"**.dev.example","upstreams":[{"node":"vps-a","address":"10.83.0.1:5"}]},
		{"host":"*x.dev.example","upstreams":[{"node":"vps-a","address":"10.83.0.1:6"}]},
		{"host":"ok.dev.example","upstreams":[{"node":"vps-a","address":"10.83.0.1:7"}]}
	]}`})

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
		if match := app.Resolve(host, "/"); match.Outcome != OutcomeUnknown {
			t.Errorf("Resolve(%q) = %q via %q, want %q", host, match.Outcome, match.Matched, OutcomeUnknown)
		}
	}

	// The well-formed entry beside them is unaffected.
	if match := app.Resolve("ok.dev.example", "/"); match.Outcome != OutcomeRoute {
		t.Errorf("outcome = %q, want %q", match.Outcome, OutcomeRoute)
	}
}

// TestIndexRepeatedWildcardKeepsTheFirst mirrors the rule for repeated names:
// the document holds each key once, and a daemon that repeats one anyway must
// not make the answer depend on how far the scan got.
func TestIndexRepeatedWildcardKeepsTheFirst(t *testing.T) {
	app := testApp(t, testFleet{name: "one", document: `{"v":1,"routes":[
		{"host":"*.dev.example","upstreams":[{"node":"vps-a","address":"10.83.0.1:41001"}]},
		{"host":"*.DEV.example","upstreams":[{"node":"vps-b","address":"10.83.0.2:41001"}]}
	]}`})

	match := app.Resolve("one.dev.example", "/")
	if match.Outcome != OutcomeRoute || match.Matched != "*.dev.example" {
		t.Fatalf("outcome = %q via %q, want %q via %q", match.Outcome, match.Matched, OutcomeRoute, "*.dev.example")
	}
	if got := dials(match.Upstreams); strings.Join(got, ",") != "10.83.0.1:41001" {
		t.Errorf("dials = %v, want the first entry's", got)
	}
}

func TestIndexWithoutAPISection(t *testing.T) {
	// A node that does not publish the fleet API sends no api member at all,
	// and every host it does not route is simply unknown.
	app := testApp(t, testFleet{name: "one", document: `{"v":1,"routes":[
		{"host":"app.dev.example","application":"p",
		 "upstreams":[{"node":"vps-a","address":"10.83.0.1:41002"}]}]}`})

	if match := app.Resolve("dev.example", "/v1/storage"); match.Outcome != OutcomeUnknown {
		t.Errorf("outcome = %q, want %q", match.Outcome, OutcomeUnknown)
	}
	if match := app.Resolve("app.dev.example", "/v1/storage"); match.Outcome != OutcomeRoute {
		t.Errorf("outcome = %q, want %q", match.Outcome, OutcomeRoute)
	}
}

func TestWildcardBasesAreNormalizedAndDeduplicated(t *testing.T) {
	doc := parseDocument(t, `{"v":1,"routes":[],
		"wildcards":["Apps.Example","apps.example","","*.bad.example","  shop.example  "]}`)
	want := []string{"apps.example", "shop.example"}
	if got := doc.WildcardBases(); !equalStrings(got, want) {
		t.Errorf("WildcardBases = %v, want %v", got, want)
	}
}

func TestADocumentWithoutWildcardsAsksForNone(t *testing.T) {
	// An older node publishes no wildcards member at all, and must keep
	// working: nothing is managed for it and its names are covered on demand.
	doc := parseDocument(t, `{"v":1,"routes":[{"host":"a.example","upstreams":[]}]}`)
	if got := doc.WildcardBases(); len(got) != 0 {
		t.Errorf("WildcardBases = %v, want none", got)
	}
}

func TestDeniesPath(t *testing.T) {
	doc := parseDocument(t, `{"v":1,"routes":[],"api":{"hosts":["p.example"],
		"upstreams":[],"denied":["/v1/storage/","/v1/ingress/routes"]}}`)

	tests := []struct {
		path   string
		denied bool
	}{
		{"/v1/storage", true},
		{"/v1/storage/", true},
		{"/v1/storage/objects/x", true},
		{"/v1/storagex", false},
		{"/v1/ingress/routes", true},
		{"/v1/ingress/routes/traefik", true},
		{"/v1/ingress/ask", false},
		{"/v1/ingress/../ingress/routes", true},
		{"/", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := doc.DeniesPath(tt.path); got != tt.denied {
			t.Errorf("DeniesPath(%q) = %v, want %v", tt.path, got, tt.denied)
		}
	}
}
