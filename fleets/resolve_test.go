package fleets

import (
	"testing"
)

// oneFleetDocument is the routing document the single-fleet resolution tests
// answer from: application hosts, one of them sharing a name with a platform
// apex, two wildcard routes with exact hosts and a platform apex under them,
// and a fleet API section with the daemon's own denied prefixes.
const oneFleetDocument = `{
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

func TestResolveWithinOneFleet(t *testing.T) {
	app := testApp(t, testFleet{name: "only", document: oneFleetDocument})

	tests := []struct {
		name    string
		host    string
		path    string
		outcome Outcome
		matched string
		dials   []string
	}{
		{
			name:    "an exact route answers with its upstreams in document order",
			host:    "app.dev.example",
			path:    "/",
			outcome: OutcomeRoute,
			matched: "app.dev.example",
			dials:   []string{"10.83.0.1:41002", "10.83.0.2:41002"},
		},
		{
			name:    "a host is matched case-insensitively and without its port",
			host:    "APP.DEV.EXAMPLE:443",
			path:    "/",
			outcome: OutcomeRoute,
			matched: "app.dev.example",
			dials:   []string{"10.83.0.1:41002", "10.83.0.2:41002"},
		},
		{
			name:    "a route with no upstreams still answers, and does not fall through to a wildcard",
			host:    "idle.dev.example",
			path:    "/",
			outcome: OutcomeRoute,
			matched: "idle.dev.example",
			dials:   []string{},
		},
		{
			name:    "a route wins over a fleet API host of the same name",
			host:    "api.dev.example",
			path:    "/",
			outcome: OutcomeRoute,
			matched: "api.dev.example",
			dials:   []string{"10.83.0.1:41001"},
		},
		{
			name:    "a fleet API host with no route answers with the daemon's own upstreams",
			host:    "ops.dev.example",
			path:    "/v1/applications",
			outcome: OutcomeRoute,
			matched: "ops.dev.example",
			dials:   []string{"127.0.0.1:7400"},
		},
		{
			name:    "a denied prefix on a fleet API host is answered by the ingress",
			host:    "ops.dev.example",
			path:    "/v1/storage/objects/x",
			outcome: OutcomeDenied,
			matched: "ops.dev.example",
		},
		{
			name:    "a denied prefix reached through dot segments is refused too",
			host:    "ops.dev.example",
			path:    "/v1/ingress/../ingress/routes",
			outcome: OutcomeDenied,
			matched: "ops.dev.example",
		},
		{
			name:    "a path that only shares a prefix with a denied one is proxied",
			host:    "ops.dev.example",
			path:    "/v1/storagex",
			outcome: OutcomeRoute,
			matched: "ops.dev.example",
			dials:   []string{"127.0.0.1:7400"},
		},
		{
			name:    "a wildcard covers a name no route and no API host names",
			host:    "new.dev.example",
			path:    "/",
			outcome: OutcomeRoute,
			matched: "*.dev.example",
			dials:   []string{"10.83.0.3:41003"},
		},
		{
			name:    "an exact route wins over the wildcard covering it",
			host:    "pinned.shop.example",
			path:    "/",
			outcome: OutcomeRoute,
			matched: "pinned.shop.example",
			dials:   []string{"10.83.0.1:41005"},
		},
		{
			name:    "a wildcard covers exactly one label and not two",
			host:    "a.b.dev.example",
			path:    "/",
			outcome: OutcomeUnknown,
		},
		{
			name:    "a wildcard does not cover its own base",
			host:    "shop.example",
			path:    "/",
			outcome: OutcomeUnknown,
		},
		{
			name:    "a host the fleet does not serve is unknown",
			host:    "elsewhere.example",
			path:    "/",
			outcome: OutcomeUnknown,
		},
		{
			name:    "a request carrying an asterisk never matches a wildcard",
			host:    "*.dev.example",
			path:    "/",
			outcome: OutcomeUnknown,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			match := app.Resolve(NormalizeHost(test.host), test.path)
			if match.Outcome != test.outcome {
				t.Fatalf("outcome = %q, want %q", match.Outcome, test.outcome)
			}
			if match.Matched != test.matched {
				t.Errorf("matched = %q, want %q", match.Matched, test.matched)
			}
			if test.outcome == OutcomeRoute || test.outcome == OutcomeDenied {
				if match.Fleet != "only" {
					t.Errorf("fleet = %q, want %q", match.Fleet, "only")
				}
			}
			if test.dials != nil && !equalStrings(dials(match.Upstreams), test.dials) {
				t.Errorf("dials = %v, want %v", dials(match.Upstreams), test.dials)
			}
		})
	}
}

func TestResolveWithoutAnyDocumentIsUnavailable(t *testing.T) {
	app := testApp(t, testFleet{name: "a"}, testFleet{name: "b"})
	match := app.Resolve("app.dev.example", "/")
	if match.Outcome != OutcomeUnavailable {
		t.Fatalf("outcome = %q, want %q", match.Outcome, OutcomeUnavailable)
	}
	if match.Fleet != "" || match.Matched != "" {
		t.Errorf("fleet = %q, matched = %q, want both empty", match.Fleet, match.Matched)
	}
}

func TestResolveWithOneSilentFleetStillAnswersTheOther(t *testing.T) {
	app := testApp(t,
		testFleet{name: "silent"},
		testFleet{name: "talking", document: `{"v":1,"routes":[
			{"host":"a.example","application":"p","upstreams":[{"node":"n","address":"10.0.0.1:1"}]}]}`},
	)
	if match := app.Resolve("a.example", "/"); match.Fleet != "talking" {
		t.Fatalf("fleet = %q, want %q", match.Fleet, "talking")
	}
	// A host nobody names is unknown rather than unavailable once ANY fleet
	// has answered: a cold start is over, even if one daemon is still silent.
	if match := app.Resolve("b.example", "/"); match.Outcome != OutcomeUnknown {
		t.Fatalf("outcome = %q, want %q", match.Outcome, OutcomeUnknown)
	}
}

const routeA = `{"v":1,"routes":[
	{"host":"shared.example","application":"p","upstreams":[{"node":"a","address":"10.0.0.1:1"}]}]}`
const routeB = `{"v":1,"routes":[
	{"host":"shared.example","application":"p","upstreams":[{"node":"b","address":"10.0.0.2:2"}]}]}`

func TestAnExactRouteBeatsAWildcardInAnotherFleet(t *testing.T) {
	app := testApp(t,
		testFleet{name: "wild", document: `{"v":1,"routes":[
			{"host":"*.example","application":"p","upstreams":[{"node":"w","address":"10.0.0.9:9"}]}]}`},
		testFleet{name: "exact", document: routeA},
	)
	match := app.Resolve("shared.example", "/")
	if match.Fleet != "exact" {
		t.Fatalf("fleet = %q, want %q; an exact route must beat a wildcard across fleets", match.Fleet, "exact")
	}
	if match.Matched != "shared.example" {
		t.Errorf("matched = %q, want %q", match.Matched, "shared.example")
	}
}

func TestAnExactRouteBeatsAFleetAPIHostInAnotherFleet(t *testing.T) {
	app := testApp(t,
		testFleet{name: "platform", document: `{"v":1,"routes":[],"api":{
			"hosts":["shared.example"],"upstreams":[{"node":"p","address":"127.0.0.1:7400"}],"denied":[]}}`},
		testFleet{name: "app", document: routeA},
	)
	if match := app.Resolve("shared.example", "/"); match.Fleet != "app" {
		t.Fatalf("fleet = %q, want %q", match.Fleet, "app")
	}
}

func TestTheFleetThatPublishedAHostFirstKeepsIt(t *testing.T) {
	app := testApp(t,
		testFleet{name: "first", document: routeA},
		testFleet{name: "second"},
	)
	if match := app.Resolve("shared.example", "/"); match.Fleet != "first" {
		t.Fatalf("fleet = %q, want %q", match.Fleet, "first")
	}

	// The second fleet now claims the same host. The first keeps it.
	publish(t, app.fleets[1], routeB)
	match := app.Resolve("shared.example", "/")
	if match.Fleet != "first" {
		t.Fatalf("fleet = %q, want %q; a claim is sticky", match.Fleet, "first")
	}
	if got := dials(match.Upstreams); !equalStrings(got, []string{"10.0.0.1:1"}) {
		t.Errorf("dials = %v, want the first fleet's", got)
	}

	// Only when the first fleet stops publishing it does the second get it.
	withdraw(t, app.fleets[0])
	if match := app.Resolve("shared.example", "/"); match.Fleet != "second" {
		t.Fatalf("fleet = %q, want %q once the holder withdraws", match.Fleet, "second")
	}

	// And when the first comes back, the second keeps it: whoever holds it
	// holds it, so the host never flaps.
	publish(t, app.fleets[0], routeA)
	if match := app.Resolve("shared.example", "/"); match.Fleet != "second" {
		t.Fatalf("fleet = %q, want %q; the holder keeps it", match.Fleet, "second")
	}
}

func TestTwoFleetsClaimingAHostAtOnceAreOrderedByDeclaration(t *testing.T) {
	app := testApp(t,
		testFleet{name: "beta", document: routeA},
		testFleet{name: "alpha", document: routeB},
	)
	if match := app.Resolve("shared.example", "/"); match.Fleet != "beta" {
		t.Fatalf("fleet = %q, want %q: declaration order breaks a tie with no history", match.Fleet, "beta")
	}
}

func TestAWildcardAndARouteAreDifferentClaims(t *testing.T) {
	// One fleet may hold "*.example" while another holds "a.example": they are
	// different keys, and neither conflicts with the other.
	app := testApp(t,
		testFleet{name: "wild", document: `{"v":1,"routes":[
			{"host":"*.example","application":"p","upstreams":[{"node":"w","address":"10.0.0.9:9"}]}]}`},
		testFleet{name: "exact", document: `{"v":1,"routes":[
			{"host":"a.example","application":"p","upstreams":[{"node":"e","address":"10.0.0.1:1"}]}]}`},
	)
	if match := app.Resolve("a.example", "/"); match.Fleet != "exact" {
		t.Errorf("a.example resolved to %q, want %q", match.Fleet, "exact")
	}
	if match := app.Resolve("b.example", "/"); match.Fleet != "wild" {
		t.Errorf("b.example resolved to %q, want %q", match.Fleet, "wild")
	}
}

func TestCertificateAllowed(t *testing.T) {
	app := testApp(t, testFleet{name: "one", document: `{"v":1,"routes":[
		{"host":"a.example","application":"p","upstreams":[{"node":"n","address":"10.0.0.1:1"}]},
		{"host":"idle.example","application":"p","upstreams":[]},
		{"host":"*.wild.example","application":"p","upstreams":[{"node":"n","address":"10.0.0.1:2"}]}],
		"api":{"hosts":["platform.example"],"upstreams":[{"node":"n","address":"127.0.0.1:7400"}],"denied":[]}}`})

	tests := []struct {
		name    string
		subject string
		allowed bool
	}{
		{"an exact route host is allowed", "a.example", true},
		{"a route host with nothing running is still allowed", "idle.example", true},
		{"a fleet API host is allowed", "platform.example", true},
		{"a name covered only by a wildcard is refused", "anything.wild.example", false},
		{"the wildcard pattern itself is refused on demand", "*.wild.example", false},
		{"a name no fleet serves is refused", "stranger.example", false},
		{"the case of the name does not matter", "A.Example", true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fleet, ok := app.CertificateAllowed(test.subject)
			if ok != test.allowed {
				t.Fatalf("allowed = %v, want %v", ok, test.allowed)
			}
			if ok && fleet != "one" {
				t.Errorf("fleet = %q, want %q", fleet, "one")
			}
		})
	}
}

func TestCertificateIsNeverAllowedWithoutADocument(t *testing.T) {
	app := testApp(t, testFleet{name: "one"})
	if _, ok := app.CertificateAllowed("a.example"); ok {
		t.Fatal("a name was allowed while no routing document had arrived; a cold start is not a yes")
	}
}

func TestOwner(t *testing.T) {
	app := testApp(t,
		testFleet{name: "a", document: `{"v":1,"routes":[
			{"host":"a.example","application":"p","upstreams":[]},
			{"host":"*.wild-a.example","application":"p","upstreams":[]}],
			"api":{"hosts":["platform-a.example"],"upstreams":[],"denied":[]},
			"wildcards":["platform-a.example","asked.example"]}`},
		testFleet{name: "b", document: `{"v":1,"routes":[
			{"host":"b.example","application":"p","upstreams":[]}]}`},
	)

	tests := []struct {
		name   string
		domain string
		fleet  string
	}{
		{"an exact route host belongs to its fleet", "a.example", "a"},
		{"a fleet API host belongs to its fleet", "platform-a.example", "a"},
		{"a name covered by a wildcard belongs to the wildcard's fleet", "sub.wild-a.example", "a"},
		{"a wildcard subject belongs to the fleet carrying the wildcard route", "*.wild-a.example", "a"},
		{"a wildcard subject belongs to the fleet that asked for the base", "*.asked.example", "a"},
		{"a wildcard subject over a platform apex belongs to that fleet", "*.platform-a.example", "a"},
		{"another fleet's host belongs to it", "b.example", "b"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fleet, ok := app.Owner(test.domain)
			if !ok {
				t.Fatalf("no owner for %q, want %q", test.domain, test.fleet)
			}
			if fleet.Name != test.fleet {
				t.Errorf("owner = %q, want %q", fleet.Name, test.fleet)
			}
		})
	}

	if _, ok := app.Owner("stranger.example"); ok {
		t.Error("a name no fleet serves has an owner; it must have none")
	}
	if _, ok := app.Owner("*.stranger.example"); ok {
		t.Error("a wildcard no fleet asked for has an owner; it must have none")
	}
}

func TestPlainHTTPFollowsTheOwningFleet(t *testing.T) {
	app := testApp(t,
		testFleet{name: "redirecting", document: `{"v":1,"routes":[
			{"host":"secure.example","application":"p","upstreams":[]}]}`},
		testFleet{name: "plain", plain: true, document: `{"v":1,"routes":[
			{"host":"plain.example","application":"p","upstreams":[]},
			{"host":"*.plainwild.example","application":"p","upstreams":[]}],
			"api":{"hosts":["plainapi.example"],"upstreams":[],"denied":[]}}`},
	)

	tests := []struct {
		name  string
		host  string
		plain bool
	}{
		{"a host of a fleet with the redirect on is redirected", "secure.example", false},
		{"a host of a fleet with the redirect off is served plain", "plain.example", true},
		{"a fleet API host follows its fleet", "plainapi.example", true},
		{"a name covered by a wildcard follows that fleet", "x.plainwild.example", true},
		{"a host with a port is answered the same way", "plain.example:80", true},
		{"a host no fleet owns is redirected", "stranger.example", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := app.PlainHTTP(test.host); got != test.plain {
				t.Errorf("PlainHTTP(%q) = %v, want %v", test.host, got, test.plain)
			}
		})
	}
}
