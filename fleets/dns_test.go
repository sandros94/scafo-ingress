package fleets

import (
	"testing"

	"github.com/caddyserver/caddy/v2/modules/caddytls"
	"github.com/caddyserver/certmagic"
)

func TestProviderCoversAZoneAtALabelBoundary(t *testing.T) {
	provider := &DNSProvider{Zones: []string{"example.com"}}
	tests := []struct {
		base    string
		covered bool
	}{
		{"example.com", true},
		{"shop.example.com", true},
		{"a.b.example.com", true},
		{"notexample.com", false},
		{"example.com.evil.test", false},
		{"com", false},
	}
	for _, tt := range tests {
		if _, ok := provider.covers(tt.base); ok != tt.covered {
			t.Errorf("covers(%q) = %v, want %v", tt.base, ok, tt.covered)
		}
	}
}

func TestSolverFor(t *testing.T) {
	tests := []struct {
		name       string
		providers  []DNSProvider
		delegation string
		base       string
		want       string // provider name, or "-" for none
		override   string
	}{
		{
			name:      "one provider with no zones takes everything",
			providers: []DNSProvider{{Module: "bunny"}},
			base:      "apps.example.com",
			want:      "",
		},
		{
			name: "the named provider whose zone is the longest suffix wins",
			providers: []DNSProvider{
				{Name: "wide", Module: "cloudflare", Zones: []string{"example.com"}},
				{Name: "narrow", Module: "bunny", Zones: []string{"apps.example.com"}},
			},
			base: "shop.apps.example.com",
			want: "narrow",
		},
		{
			name: "a zone apex is covered by its own provider",
			providers: []DNSProvider{
				{Name: "cf", Module: "cloudflare", Zones: []string{"example.com"}},
			},
			base: "example.com",
			want: "cf",
		},
		{
			name: "a base under no zone falls to the default provider",
			providers: []DNSProvider{
				{Name: "cf", Module: "cloudflare", Zones: []string{"example.com"}},
				{Module: "bunny"},
			},
			base: "customer.test",
			want: "",
		},
		{
			name: "a base under no zone is delegated when a delegation is configured",
			providers: []DNSProvider{
				{Name: "cf", Module: "cloudflare", Zones: []string{"operator.example"}},
			},
			delegation: "acme.operator.example",
			base:       "shop.customer.test",
			want:       "cf",
			override:   "acme.operator.example",
		},
		{
			name: "the delegation is written by whoever holds the delegation name's zone",
			providers: []DNSProvider{
				{Name: "cf", Module: "cloudflare", Zones: []string{"platform.example"}},
				{Name: "bn", Module: "bunny", Zones: []string{"operator.example"}},
			},
			delegation: "acme.operator.example",
			base:       "shop.customer.test",
			want:       "bn",
			override:   "acme.operator.example",
		},
		{
			name:       "with no zone declared anywhere the default provider still takes everything",
			providers:  []DNSProvider{{Module: "bunny"}},
			delegation: "acme.operator.example",
			base:       "shop.customer.test",
			want:       "",
		},
		{
			name:      "a base under no zone and no default provider has no solver",
			providers: []DNSProvider{{Name: "cf", Module: "cloudflare", Zones: []string{"example.com"}}},
			base:      "customer.test",
			want:      "-",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app := &App{DNS: test.providers, ACMEDelegation: test.delegation}
			provider, override, ok := app.solverFor(test.base)
			if test.want == "-" {
				if ok {
					t.Fatalf("solverFor(%q) = %q, want none", test.base, provider.describe())
				}
				return
			}
			if !ok {
				t.Fatalf("solverFor(%q) found no provider, want %q", test.base, test.want)
			}
			if provider.Name != test.want {
				t.Errorf("provider = %q, want %q", provider.Name, test.want)
			}
			if override != test.override {
				t.Errorf("override = %q, want %q", override, test.override)
			}
		})
	}
}

// TestZonePatternsCoverEveryDepth is the test the wildcardDepth comment exists
// for: CertMagic's subject matcher needs one pattern per label depth, and a
// policy built from a zone has to admit the subjects a fleet will actually ask
// for.
func TestZonePatternsCoverEveryDepth(t *testing.T) {
	policy := &caddytls.AutomationPolicy{SubjectsRaw: zonePatterns("example.com")}

	covered := []string{
		"example.com",
		"*.example.com",
		"*.apps.example.com",
		"*.a.b.example.com",
		"*.a.b.c.example.com",
	}
	for _, subject := range covered {
		if !policyCovers(policy, subject) {
			t.Errorf("a policy for example.com does not cover %q, and must", subject)
		}
	}

	for _, subject := range []string{"*.elsewhere.test", "elsewhere.test", "*.notexample.com"} {
		if policyCovers(policy, subject) {
			t.Errorf("a policy for example.com covers %q, and must not", subject)
		}
	}
}

// TestMatchWildcardReplacesLabelsCumulatively pins the CertMagic behaviour the
// whole zone-pattern scheme rests on. If this ever stops holding, zonePatterns
// is wrong and the wildcard certificates stop being issued.
func TestMatchWildcardReplacesLabelsCumulatively(t *testing.T) {
	if !certmagic.MatchWildcard("*.shop.example.com", "*.*.example.com") {
		t.Error("MatchWildcard no longer replaces labels cumulatively from the left")
	}
	if certmagic.MatchWildcard("*.shop.example.com", "*.example.com") {
		t.Error("MatchWildcard now covers more than one label; zonePatterns is overspecified")
	}
}

func TestPlanSubjectsGroupsBasesByTheirSolver(t *testing.T) {
	app := testApp(t,
		testFleet{name: "a", document: `{"v":1,"routes":[],
			"api":{"hosts":["apps.example.com"],"upstreams":[],"denied":[]},
			"wildcards":["apps.example.com","shop.customer.test"]}`},
		testFleet{name: "b", document: `{"v":1,"routes":[],"wildcards":["other.example.com"]}`},
	)
	app.DNS = []DNSProvider{{Name: "cf", Module: "cloudflare", Zones: []string{"example.com"}}}
	app.ACMEDelegation = "acme.example.com"

	plan := app.planSubjects()

	own := plan.byGroup[groupKey{provider: "cf"}]
	if _, ok := own["*.apps.example.com"]; !ok {
		t.Error("the platform wildcard is not in the operator's own group")
	}
	if _, ok := own["apps.example.com"]; !ok {
		t.Error("the bare platform apex is not managed although the fleet API answers on it")
	}
	if _, ok := own["*.other.example.com"]; !ok {
		t.Error("another fleet's base in the same zone is not in the group")
	}

	delegated := plan.byGroup[groupKey{provider: "cf", override: "acme.example.com"}]
	if _, ok := delegated["*.shop.customer.test"]; !ok {
		t.Error("a customer domain under no zone was not delegated")
	}
	if _, ok := delegated["shop.customer.test"]; ok {
		t.Error("a customer domain's bare apex was managed although no fleet API answers on it")
	}
}

func TestPlanSubjectsReportsABaseNothingCanSolve(t *testing.T) {
	app := testApp(t, testFleet{name: "a", document: `{"v":1,"routes":[],
		"wildcards":["shop.customer.test"]}`})
	app.DNS = []DNSProvider{{Name: "cf", Module: "cloudflare", Zones: []string{"example.com"}}}

	plan := app.planSubjects()
	if _, ok := plan.unsolvable["shop.customer.test"]; !ok {
		t.Fatal("a base under no zone, with no default provider and no delegation, was not reported")
	}
	if len(plan.byGroup) != 0 {
		t.Errorf("byGroup = %v, want nothing to manage", plan.byGroup)
	}
}

func TestDelegationRecordIsOneSentence(t *testing.T) {
	got := DelegationRecord("shop.customer.test", "acme.operator.example")
	want := "_acme-challenge.shop.customer.test CNAME acme.operator.example"
	if got != want {
		t.Errorf("DelegationRecord = %q, want %q", got, want)
	}
}
