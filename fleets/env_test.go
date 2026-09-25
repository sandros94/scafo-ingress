package fleets

import (
	"strings"
	"testing"
)

func TestFleetsFromEnv(t *testing.T) {
	tests := []struct {
		name string
		env  environment
		want []Fleet
	}{
		{
			name: "no scafo variable at all is no fleet, not an error",
			env:  environment{"PATH": "/usr/bin", "SCAFO_ACME_EMAIL": "ops@example"},
		},
		{
			name: "the single form makes one fleet called default",
			env: environment{
				"SCAFO_FLEET_ENDPOINT": "http://127.0.0.1:7400",
				"SCAFO_FLEET_TOKEN":    "t",
			},
			want: []Fleet{{Name: "default"}},
		},
		{
			name: "the single form takes a token file",
			env: environment{
				"SCAFO_FLEET_ENDPOINT":   "http://127.0.0.1:7400",
				"SCAFO_FLEET_TOKEN_FILE": "/run/token",
			},
			want: []Fleet{{Name: "default"}},
		},
		{
			name: "a named fleet lowercases and hyphenates its name",
			env: environment{
				"SCAFO_FLEET_CLIENT_A_ENDPOINT": "http://10.0.0.1:7400",
				"SCAFO_FLEET_CLIENT_A_TOKEN":    "a",
			},
			want: []Fleet{{Name: "client-a"}},
		},
		{
			name: "several named fleets come back sorted by name",
			env: environment{
				"SCAFO_FLEET_B_ENDPOINT": "http://10.0.0.2:7400",
				"SCAFO_FLEET_B_TOKEN":    "b",
				"SCAFO_FLEET_A_ENDPOINT": "http://10.0.0.1:7400",
				"SCAFO_FLEET_A_TOKEN":    "a",
			},
			want: []Fleet{{Name: "a"}, {Name: "b"}},
		},
		{
			name: "a fleet name may contain what looks like a suffix",
			env: environment{
				"SCAFO_FLEET_A_TOKEN_ENDPOINT": "http://10.0.0.1:7400",
				"SCAFO_FLEET_A_TOKEN_TOKEN":    "t",
			},
			want: []Fleet{{Name: "a-token"}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := fleetsFromEnv(test.env)
			if err != nil {
				t.Fatalf("fleetsFromEnv: %v", err)
			}
			if len(got) != len(test.want) {
				t.Fatalf("got %d fleets (%v), want %d", len(got), names(got), len(test.want))
			}
			for i := range got {
				if got[i].Name != test.want[i].Name {
					t.Errorf("fleet %d = %q, want %q", i, got[i].Name, test.want[i].Name)
				}
			}
		})
	}
}

func TestFleetsFromEnvReadsEverySetting(t *testing.T) {
	got, err := fleetsFromEnv(environment{
		"SCAFO_FLEET_ENDPOINT":        "http://127.0.0.1:7400",
		"SCAFO_FLEET_TOKEN":           " t ",
		"SCAFO_FLEET_PREFER":          "vps-b",
		"SCAFO_FLEET_HTTP_REDIRECT":   "off",
		"SCAFO_FLEET_POLL_INTERVAL":   "2s",
		"SCAFO_FLEET_TIMEOUT":         "3s",
		"SCAFO_FLEET_STORAGE_TIMEOUT": "4s",
	})
	if err != nil {
		t.Fatalf("fleetsFromEnv: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d fleets, want 1", len(got))
	}
	fleet := got[0]
	if fleet.Endpoint != "http://127.0.0.1:7400" || fleet.Token != "t" || fleet.Prefer != "vps-b" {
		t.Errorf("connection settings = %+v", fleet.Config)
	}
	if fleet.HTTPRedirect == nil || *fleet.HTTPRedirect {
		t.Errorf("http_redirect = %v, want off", fleet.HTTPRedirect)
	}
	if fleet.PollInterval == 0 || fleet.Timeout == 0 || fleet.StorageTimeout == 0 {
		t.Errorf("durations = %v %v %v, want all set", fleet.PollInterval, fleet.Timeout, fleet.StorageTimeout)
	}
}

func TestFleetsFromEnvErrors(t *testing.T) {
	tests := []struct {
		name    string
		env     environment
		wantErr string
	}{
		{
			name: "the single and the named form together",
			env: environment{
				"SCAFO_FLEET_ENDPOINT":   "http://127.0.0.1:7400",
				"SCAFO_FLEET_TOKEN":      "t",
				"SCAFO_FLEET_A_ENDPOINT": "http://10.0.0.1:7400",
				"SCAFO_FLEET_A_TOKEN":    "a",
			},
			wantErr: "cannot be used together",
		},
		{
			name: "the single form beside a fleet called default, which is the same fleet twice",
			env: environment{
				"SCAFO_FLEET_ENDPOINT":         "http://127.0.0.1:7400",
				"SCAFO_FLEET_TOKEN":            "t",
				"SCAFO_FLEET_DEFAULT_ENDPOINT": "http://10.0.0.1:7400",
			},
			wantErr: "cannot be used together",
		},
		{
			name: "an endpoint with no token",
			env: environment{
				"SCAFO_FLEET_A_ENDPOINT": "http://10.0.0.1:7400",
			},
			wantErr: "has an endpoint and no token",
		},
		{
			name: "a token with no endpoint",
			env: environment{
				"SCAFO_FLEET_A_TOKEN": "a",
			},
			wantErr: "has no endpoint",
		},
		{
			name: "a setting this build does not know",
			env: environment{
				"SCAFO_FLEET_A_ENDPONT": "http://10.0.0.1:7400",
			},
			wantErr: "is not a fleet setting this build knows",
		},
		{
			name: "a name that is not a slug once mapped",
			env: environment{
				"SCAFO_FLEET__A_ENDPOINT": "http://10.0.0.1:7400",
				"SCAFO_FLEET__A_TOKEN":    "a",
			},
			wantErr: "is not a slug",
		},
		{
			name: "an unreadable boolean",
			env: environment{
				"SCAFO_FLEET_ENDPOINT":      "http://127.0.0.1:7400",
				"SCAFO_FLEET_TOKEN":         "t",
				"SCAFO_FLEET_HTTP_REDIRECT": "maybe",
			},
			wantErr: "expected on or off",
		},
		{
			name: "a duration that is not one",
			env: environment{
				"SCAFO_FLEET_ENDPOINT":      "http://127.0.0.1:7400",
				"SCAFO_FLEET_TOKEN":         "t",
				"SCAFO_FLEET_POLL_INTERVAL": "soon",
			},
			wantErr: "parsing duration",
		},
		{
			name: "a duration that is not positive",
			env: environment{
				"SCAFO_FLEET_ENDPOINT":      "http://127.0.0.1:7400",
				"SCAFO_FLEET_TOKEN":         "t",
				"SCAFO_FLEET_POLL_INTERVAL": "0s",
			},
			wantErr: "must be positive",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := fleetsFromEnv(test.env)
			if err == nil {
				t.Fatalf("error = nil, want one mentioning %q", test.wantErr)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, test.wantErr)
			}
		})
	}
}

func TestFleetsFromEnvErrorsNameTheVariables(t *testing.T) {
	_, err := fleetsFromEnv(environment{
		"SCAFO_FLEET_ENDPOINT":   "http://127.0.0.1:7400",
		"SCAFO_FLEET_TOKEN":      "t",
		"SCAFO_FLEET_A_ENDPOINT": "http://10.0.0.1:7400",
		"SCAFO_FLEET_A_TOKEN":    "a",
	})
	if err == nil {
		t.Fatal("error = nil")
	}
	for _, variable := range []string{"SCAFO_FLEET_ENDPOINT", "SCAFO_FLEET_TOKEN", "SCAFO_FLEET_A_ENDPOINT", "SCAFO_FLEET_A_TOKEN"} {
		if !strings.Contains(err.Error(), variable) {
			t.Errorf("error does not name %s: %q", variable, err)
		}
	}
}

func TestDNSProvidersFromEnv(t *testing.T) {
	t.Run("a default provider and its fields", func(t *testing.T) {
		got, err := dnsProvidersFromEnv(environment{
			"SCAFO_DNS_PROVIDER": "bunny",
			"SCAFO_DNS_API_KEY":  "k",
		})
		if err != nil {
			t.Fatalf("dnsProvidersFromEnv: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d providers, want 1", len(got))
		}
		if got[0].Name != "" || got[0].Module != "bunny" {
			t.Errorf("provider = %+v, want the default bunny one", got[0])
		}
		if got[0].Fields["api_key"] != "k" {
			t.Errorf("fields = %v, want api_key=k", got[0].Fields)
		}
	})

	t.Run("a named provider carves its fields out of the default namespace", func(t *testing.T) {
		got, err := dnsProvidersFromEnv(environment{
			"SCAFO_DNS_PROVIDER":       "bunny",
			"SCAFO_DNS_API_KEY":        "k",
			"SCAFO_DNS_CF_PROVIDER":    "cloudflare",
			"SCAFO_DNS_CF_ZONES":       "a.example, b.example",
			"SCAFO_DNS_CF_API_TOKEN":   "c",
			"SCAFO_DNS_CF_EXTRA_THING": "x",
		})
		if err != nil {
			t.Fatalf("dnsProvidersFromEnv: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d providers, want 2", len(got))
		}
		named, fallback := got[0], got[1]
		if named.Name != "cf" || named.Module != "cloudflare" {
			t.Errorf("named provider = %+v", named)
		}
		if !equalStrings(named.Zones, []string{"a.example", "b.example"}) {
			t.Errorf("zones = %v, want the two listed", named.Zones)
		}
		if named.Fields["api_token"] != "c" || named.Fields["extra_thing"] != "x" {
			t.Errorf("named fields = %v", named.Fields)
		}
		if fallback.Module != "bunny" || fallback.Fields["api_key"] != "k" {
			t.Errorf("default provider = %+v", fallback)
		}
		if _, leaked := fallback.Fields["cf_api_token"]; leaked {
			t.Error("a named provider's field leaked into the default provider's")
		}
	})

	t.Run("the longest provider name claims an ambiguous variable", func(t *testing.T) {
		got, err := dnsProvidersFromEnv(environment{
			"SCAFO_DNS_A_PROVIDER":    "bunny",
			"SCAFO_DNS_A_B_PROVIDER":  "cloudflare",
			"SCAFO_DNS_A_B_API_TOKEN": "t",
		})
		if err != nil {
			t.Fatalf("dnsProvidersFromEnv: %v", err)
		}
		byName := map[string]DNSProvider{}
		for _, provider := range got {
			byName[provider.Name] = provider
		}
		if byName["a-b"].Fields["api_token"] != "t" {
			t.Errorf("a-b fields = %v, want api_token=t", byName["a-b"].Fields)
		}
		if _, wrong := byName["a"].Fields["b_api_token"]; wrong {
			t.Error("the shorter provider name claimed the longer one's field")
		}
	})

	t.Run("a field with no provider is refused", func(t *testing.T) {
		_, err := dnsProvidersFromEnv(environment{"SCAFO_DNS_API_KEY": "k"})
		if err == nil || !strings.Contains(err.Error(), "no provider is declared") {
			t.Fatalf("error = %v, want one saying no provider is declared", err)
		}
	})

	t.Run("nothing configured is no provider, not an error", func(t *testing.T) {
		got, err := dnsProvidersFromEnv(environment{"PATH": "/usr/bin"})
		if err != nil || got != nil {
			t.Fatalf("got %v, %v; want nothing", got, err)
		}
	})
}

// names lists fleet names for a failure message.
func names(fleets []Fleet) []string {
	out := make([]string, 0, len(fleets))
	for _, fleet := range fleets {
		out = append(out, fleet.Name)
	}
	return out
}
