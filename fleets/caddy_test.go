package fleets

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
)

// parseGlobal runs the scafo global option over a Caddyfile fragment and
// returns the app it produced.
func parseGlobal(t *testing.T, input string) (*App, error) {
	t.Helper()
	d := caddyfile.NewTestDispenser(input)
	value, err := parseGlobalOption(d, nil)
	if err != nil {
		return nil, err
	}
	app := value.(httpcaddyfile.App)
	if app.Name != AppName {
		t.Fatalf("app name = %q, want %q", app.Name, AppName)
	}
	var parsed App
	if err := json.Unmarshal(app.Value, &parsed); err != nil {
		t.Fatalf("decoding the adapted app: %v", err)
	}
	return &parsed, nil
}

func TestGlobalOptionParsesFleets(t *testing.T) {
	app, err := parseGlobal(t, `scafo {
		fleet client-a {
			endpoint        http://10.0.0.1:7400
			token           a-token
			prefer          vps-b
			poll_interval   2s
			timeout         3s
			storage_timeout 4s
			http_redirect   off
		}
		fleet client-b {
			endpoint   http://10.0.0.2:7400
			token_file /run/b
		}
	}`)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if len(app.FleetsRaw) != 2 {
		t.Fatalf("got %d fleets, want 2", len(app.FleetsRaw))
	}
	a := app.FleetsRaw[0]
	if a.Name != "client-a" || a.Endpoint != "http://10.0.0.1:7400" || a.Token != "a-token" {
		t.Errorf("fleet a = %+v", a)
	}
	if a.Prefer != "vps-b" {
		t.Errorf("prefer = %q, want vps-b", a.Prefer)
	}
	if a.HTTPRedirect == nil || *a.HTTPRedirect {
		t.Errorf("http_redirect = %v, want off", a.HTTPRedirect)
	}
	if time := caddy.Duration(2e9); a.PollInterval != time {
		t.Errorf("poll_interval = %v, want 2s", a.PollInterval)
	}
	b := app.FleetsRaw[1]
	if b.Name != "client-b" || b.TokenFile != "/run/b" {
		t.Errorf("fleet b = %+v", b)
	}
	if b.HTTPRedirect != nil {
		t.Errorf("http_redirect = %v, want unset (which means on)", b.HTTPRedirect)
	}
}

func TestGlobalOptionParsesDNSProviders(t *testing.T) {
	app, err := parseGlobal(t, `scafo {
		dns bunny {
			api_key bunny-key
		}
		dns cf cloudflare {
			zones     a.example b.example
			api_token cf-token
		}
		acme_delegation acme.operator.example
		acme_email      ops@operator.example
		acme_ca         https://localhost:14000/dir
	}`)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if len(app.DNS) != 2 {
		t.Fatalf("got %d providers, want 2", len(app.DNS))
	}
	fallback, named := app.DNS[0], app.DNS[1]
	if fallback.Name != "" || fallback.Module != "bunny" || fallback.Fields["api_key"] != "bunny-key" {
		t.Errorf("default provider = %+v", fallback)
	}
	if named.Name != "cf" || named.Module != "cloudflare" || named.Fields["api_token"] != "cf-token" {
		t.Errorf("named provider = %+v", named)
	}
	if !equalStrings(named.Zones, []string{"a.example", "b.example"}) {
		t.Errorf("zones = %v", named.Zones)
	}
	if app.ACMEDelegation != "acme.operator.example" || app.ACMEEmail != "ops@operator.example" {
		t.Errorf("acme settings = %q %q", app.ACMEDelegation, app.ACMEEmail)
	}
	if app.ACMECA != "https://localhost:14000/dir" {
		t.Errorf("acme_ca = %q", app.ACMECA)
	}
}

func TestGlobalOptionAcceptsAnEmptyBlock(t *testing.T) {
	// The image's default Caddyfile carries exactly this: the app is declared
	// so the environment can fill it in.
	app, err := parseGlobal(t, `scafo {
	}`)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if len(app.FleetsRaw) != 0 || len(app.DNS) != 0 {
		t.Errorf("an empty block produced %+v", app)
	}
}

func TestGlobalOptionErrors(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr string
	}{
		{
			name:    "an unknown option",
			input:   "scafo {\n\tnonsense x\n}",
			wantErr: "unrecognized scafo option",
		},
		{
			name:    "an unknown fleet option",
			input:   "scafo {\n\tfleet a {\n\t\tnonsense x\n\t}\n}",
			wantErr: "unrecognized scafo fleet option",
		},
		{
			name:    "a fleet with no name",
			input:   "scafo {\n\tfleet {\n\t\tendpoint http://x\n\t}\n}",
			wantErr: "a fleet must be named",
		},
		{
			name:    "a poll interval that is not positive",
			input:   "scafo {\n\tfleet a {\n\t\tpoll_interval 0s\n\t}\n}",
			wantErr: "must be positive",
		},
		{
			name:    "an unreadable http_redirect",
			input:   "scafo {\n\tfleet a {\n\t\thttp_redirect maybe\n\t}\n}",
			wantErr: "expected on or off",
		},
		{
			name:    "dns with too many arguments",
			input:   "scafo {\n\tdns a b c\n}",
			wantErr: "dns takes either a module name",
		},
		{
			name:    "a dns field set twice",
			input:   "scafo {\n\tdns bunny {\n\t\tapi_key a\n\t\tapi_key b\n\t}\n}",
			wantErr: "is set twice",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseGlobal(t, test.input)
			if err == nil {
				t.Fatalf("error = nil, want one mentioning %q", test.wantErr)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, test.wantErr)
			}
		})
	}
}

func TestFleetResolveDefaults(t *testing.T) {
	fleet := &Fleet{Name: "a"}
	fleet.Endpoint = "http://127.0.0.1:7400/"
	fleet.Token = "  t  "
	if err := fleet.resolve(); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if fleet.BearerToken() != "t" {
		t.Errorf("token = %q, want it trimmed", fleet.BearerToken())
	}
	if got := BaseURL(fleet.EndpointURL()); got != "http://127.0.0.1:7400" {
		t.Errorf("base = %q, want the trailing slash gone", got)
	}
	if fleet.Prefer != DefaultPrefer {
		t.Errorf("prefer = %q, want %q", fleet.Prefer, DefaultPrefer)
	}
	if !fleet.RedirectsHTTP() {
		t.Error("http_redirect defaulted to off, want on")
	}
	if fleet.StorageRequestTimeout() != DefaultStorageTimeout {
		t.Errorf("storage timeout = %v, want %v", fleet.StorageRequestTimeout(), DefaultStorageTimeout)
	}
}

func TestFleetResolveErrors(t *testing.T) {
	tests := []struct {
		name    string
		fleet   Fleet
		wantErr string
	}{
		{"no name", Fleet{}, "a fleet must be named"},
		{"a name that is not a slug", Fleet{Name: "Client_A"}, "is not a slug"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fleet := test.fleet
			err := fleet.resolve()
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want one mentioning %q", err, test.wantErr)
			}
		})
	}

	both := Fleet{Name: "a"}
	both.Endpoint = "http://127.0.0.1:7400"
	both.Token = "t"
	both.TokenFile = "/run/t"
	if err := both.resolve(); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error = %v, want one about token and token_file", err)
	}

	neither := Fleet{Name: "a"}
	neither.Endpoint = "http://127.0.0.1:7400"
	if err := neither.resolve(); err == nil || !strings.Contains(err.Error(), "either token or token_file") {
		t.Errorf("error = %v, want one about a missing token", err)
	}
}

func TestCaddyModuleID(t *testing.T) {
	if got := (&App{}).CaddyModule().ID; got != AppName {
		t.Errorf("module ID = %q, want %q", got, AppName)
	}
	if _, ok := (&App{}).CaddyModule().New().(*App); !ok {
		t.Error("New() does not return an *App")
	}
}
