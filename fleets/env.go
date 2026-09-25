package fleets

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/caddyserver/caddy/v2"
)

// The environment is a first-class way to configure this app, not a fallback
// for the impatient: the deployment this repository exists for hands an
// operator a container and nothing else, with no Caddyfile to upload and no
// volume to bind. Everything the app needs therefore has an environment form,
// and the default Caddyfile in the image declares an EMPTY scafo block so that
// the form below is what fills it in.
//
// The rule that makes this safe is that nothing here guesses. A variable this
// build does not recognize, a fleet with an endpoint and no token, the single
// and the named form used together -- each is a provisioning error naming the
// variables involved, because a mis-spelled variable that is quietly ignored
// is a fleet that quietly does not exist.
const (
	// fleetPrefix introduces every fleet variable.
	fleetPrefix = "SCAFO_FLEET_"

	// dnsPrefix introduces every DNS provider variable.
	dnsPrefix = "SCAFO_DNS_"

	// acmeDelegationVar, acmeEmailVar and acmeCAVar are the ACME settings.
	acmeDelegationVar = "SCAFO_ACME_DELEGATION"
	acmeEmailVar      = "SCAFO_ACME_EMAIL"
	acmeCAVar         = "SCAFO_ACME_CA"

	// defaultFleetName is the name the single-fleet form produces.
	defaultFleetName = "default"
)

// fleetSuffixes are the settings a fleet variable may name, LONGEST FIRST.
//
// The order is the parser: a variable is split by trying these in turn, so
// SCAFO_FLEET_A_TOKEN_FILE is fleet "a"'s token_file rather than fleet
// "a-token"'s file, while SCAFO_FLEET_A_TOKEN_ENDPOINT is fleet "a-token"'s
// endpoint. Matching a known suffix rather than splitting on the last
// underscore is what lets a fleet name contain one.
var fleetSuffixes = []string{
	"_STORAGE_TIMEOUT",
	"_HTTP_REDIRECT",
	"_POLL_INTERVAL",
	"_TOKEN_FILE",
	"_ENDPOINT",
	"_TIMEOUT",
	"_PREFER",
	"_TOKEN",
}

// envNameRE is the shape of the <NAME> part of a named variable. A name that
// is not this shape is a mistake rather than a fleet.
var envNameRE = regexp.MustCompile(`^[A-Z0-9_]+$`)

// environment is the process environment as a map, so tests can supply one.
type environment map[string]string

// osEnvironment reads the process environment.
func osEnvironment() environment {
	env := environment{}
	for _, entry := range os.Environ() {
		if name, value, ok := strings.Cut(entry, "="); ok {
			env[name] = value
		}
	}
	return env
}

// fleetName maps the <NAME> part of a variable onto a fleet name: lowercased,
// with underscores becoming hyphens. SCAFO_FLEET_CLIENT_A_ENDPOINT is fleet
// "client-a".
func fleetName(envName string) string {
	return strings.ReplaceAll(strings.ToLower(envName), "_", "-")
}

// fleetsFromEnv builds the app's fleets from the environment.
//
// It is called only when the configuration declares no fleet of its own: a
// Caddyfile that states its fleets states all of them, and the environment is
// then not consulted at all, so the two can never be half-merged.
func fleetsFromEnv(env environment) ([]Fleet, error) {
	// Collect every variable that is ours, so the errors below can name them
	// and so a variable that is nearly right is refused rather than ignored.
	type setting struct {
		variable string
		envName  string // the <NAME> part, empty for the single-fleet form
		suffix   string
		value    string
	}
	var settings []setting
	for variable, value := range env {
		if !strings.HasPrefix(variable, fleetPrefix) {
			continue
		}
		rest := strings.TrimPrefix(variable, fleetPrefix)
		matched := false
		for _, suffix := range fleetSuffixes {
			// The single-fleet form is the suffix with nothing before it --
			// SCAFO_FLEET_TOKEN -- and the named form is a name and the
			// suffix: SCAFO_FLEET_A_TOKEN. The single form is tried first, so
			// SCAFO_FLEET_STORAGE_TIMEOUT is the default fleet's storage
			// timeout rather than a fleet called "storage" timing out; the
			// two forms cannot be used together anyway.
			if rest == strings.TrimPrefix(suffix, "_") {
				settings = append(settings, setting{variable, "", suffix, value})
				matched = true
				break
			}
			trimmed, found := strings.CutSuffix(rest, suffix)
			if !found || trimmed == "" {
				continue
			}
			if !envNameRE.MatchString(trimmed) {
				return nil, fmt.Errorf("%s: %s names a fleet %q that is not [A-Z0-9_]+", errPrefix, variable, trimmed)
			}
			settings = append(settings, setting{variable, trimmed, suffix, value})
			matched = true
			break
		}
		if !matched {
			return nil, fmt.Errorf("%s: %s is not a fleet setting this build knows; the settings are %s",
				errPrefix, variable, strings.Join(knownFleetSettings(), ", "))
		}
	}
	if len(settings) == 0 {
		return nil, nil
	}

	// The single form (SCAFO_FLEET_TOKEN) and the named form
	// (SCAFO_FLEET_A_TOKEN) are mutually exclusive. They are the one pair that
	// can map two variables onto one fleet -- SCAFO_FLEET_ENDPOINT and
	// SCAFO_FLEET_DEFAULT_ENDPOINT are both "default" -- and guessing which
	// one was meant is exactly what this must not do.
	var single, named []string
	for _, s := range settings {
		if s.envName == "" {
			single = append(single, s.variable)
		} else {
			named = append(named, s.variable)
		}
	}
	if len(single) > 0 && len(named) > 0 {
		sort.Strings(single)
		sort.Strings(named)
		return nil, fmt.Errorf("%s: the single-fleet form (%s) and the named form (%s) cannot be used together; state every fleet the same way",
			errPrefix, strings.Join(single, ", "), strings.Join(named, ", "))
	}

	// Group by fleet, remembering which variable set each value so an error
	// can name it.
	type draft struct {
		fleet Fleet
		from  map[string]string // suffix -> variable
	}
	drafts := map[string]*draft{}
	order := []string{}
	for _, s := range settings {
		name := defaultFleetName
		if s.envName != "" {
			name = fleetName(s.envName)
		}
		if !ValidName(name) {
			return nil, fmt.Errorf("%s: %s names the fleet %q, which is not a slug (lowercase alphanumerics separated by single hyphens)", errPrefix, s.variable, name)
		}
		d, ok := drafts[name]
		if !ok {
			d = &draft{fleet: Fleet{Name: name}, from: map[string]string{}}
			drafts[name] = d
			order = append(order, name)
		}
		if previous, taken := d.from[s.suffix]; taken {
			return nil, fmt.Errorf("%s: %s and %s both set %s of fleet %q", errPrefix, previous, s.variable, strings.ToLower(strings.TrimPrefix(s.suffix, "_")), name)
		}
		d.from[s.suffix] = s.variable
		if err := applyFleetSetting(&d.fleet, s.suffix, s.value, s.variable); err != nil {
			return nil, err
		}
	}

	sort.Strings(order)
	built := make([]Fleet, 0, len(order))
	for _, name := range order {
		d := drafts[name]
		// A fleet with an endpoint and no credential is a provisioning
		// mistake, not a fleet to try anyway: it would fail every fetch and
		// every storage call with a 401 and look like a daemon that is down.
		if d.fleet.Endpoint == "" {
			return nil, fmt.Errorf("%s: fleet %q has no endpoint; set %s", errPrefix, name, variableFor(name, "_ENDPOINT"))
		}
		if d.fleet.Token == "" && d.fleet.TokenFile == "" {
			return nil, fmt.Errorf("%s: fleet %q has an endpoint and no token; set %s or %s",
				errPrefix, name, variableFor(name, "_TOKEN"), variableFor(name, "_TOKEN_FILE"))
		}
		built = append(built, d.fleet)
	}
	return built, nil
}

// variableFor spells the variable that would set one of a fleet's settings,
// for an error message. A fleet named "default" may have come from either
// form, and the single one is the friendlier suggestion.
func variableFor(name, suffix string) string {
	if name == defaultFleetName {
		return fleetPrefix + strings.TrimPrefix(suffix, "_")
	}
	return fleetPrefix + strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + suffix
}

// knownFleetSettings lists the fleet variables, for the error a mis-spelled
// one produces.
func knownFleetSettings() []string {
	known := make([]string, 0, len(fleetSuffixes))
	for _, suffix := range fleetSuffixes {
		known = append(known, fleetPrefix+"[<NAME>_]"+strings.TrimPrefix(suffix, "_"))
	}
	sort.Strings(known)
	return known
}

// applyFleetSetting writes one environment value onto a fleet.
func applyFleetSetting(fleet *Fleet, suffix, value, variable string) error {
	value = strings.TrimSpace(value)
	switch suffix {
	case "_ENDPOINT":
		fleet.Endpoint = value
	case "_TOKEN":
		fleet.Token = value
	case "_TOKEN_FILE":
		fleet.TokenFile = value
	case "_PREFER":
		fleet.Prefer = value
	case "_HTTP_REDIRECT":
		on, err := parseBool(value)
		if err != nil {
			return fmt.Errorf("%s: %s: %v", errPrefix, variable, err)
		}
		fleet.HTTPRedirect = &on
	case "_POLL_INTERVAL", "_TIMEOUT", "_STORAGE_TIMEOUT":
		duration, err := caddy.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("%s: %s: parsing duration %q: %v", errPrefix, variable, value, err)
		}
		if duration <= 0 {
			return fmt.Errorf("%s: %s must be positive, got %s", errPrefix, variable, duration)
		}
		switch suffix {
		case "_POLL_INTERVAL":
			fleet.PollInterval = caddy.Duration(duration)
		case "_TIMEOUT":
			fleet.Timeout = caddy.Duration(duration)
		default:
			fleet.StorageTimeout = caddy.Duration(duration)
		}
	}
	return nil
}

// parseBool reads the on/off vocabulary these variables use. Anything else is
// refused rather than read as "off", because a typo that silently disables a
// redirect is the kind of thing nobody notices for a month.
func parseBool(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "on", "true", "yes", "1":
		return true, nil
	case "off", "false", "no", "0":
		return false, nil
	}
	return false, fmt.Errorf("expected on or off, got %q", value)
}

// dnsProvidersFromEnv builds the DNS provider configuration from the
// environment.
//
// The default provider is SCAFO_DNS_PROVIDER, and every other SCAFO_DNS_<X>
// variable is one of its fields -- SCAFO_DNS_API_KEY is "api_key" -- unless
// <X> starts with the name of a provider that SCAFO_DNS_<NAME>_PROVIDER
// declared. Declaring the name is therefore what carves a field namespace out
// of the default one, and there is no other way for the parser to tell
// SCAFO_DNS_CF_API_TOKEN (provider "cf", field "api_token") from a default
// provider with a field called "cf_api_token".
func dnsProvidersFromEnv(env environment) ([]DNSProvider, error) {
	// First pass: which named providers exist at all.
	namedModules := map[string]string{}
	var defaultModule string
	var anyVariable bool
	for variable, value := range env {
		if !strings.HasPrefix(variable, dnsPrefix) {
			continue
		}
		anyVariable = true
		rest := strings.TrimPrefix(variable, dnsPrefix)
		if rest == "PROVIDER" {
			defaultModule = strings.TrimSpace(value)
			continue
		}
		name, found := strings.CutSuffix(rest, "_PROVIDER")
		if !found || name == "" {
			continue
		}
		if !envNameRE.MatchString(name) {
			return nil, fmt.Errorf("%s: %s names a provider %q that is not [A-Z0-9_]+", errPrefix, variable, name)
		}
		namedModules[name] = strings.TrimSpace(value)
	}
	if defaultModule == "" && len(namedModules) == 0 {
		if !anyVariable {
			return nil, nil
		}
		// Variables that configure a provider, with no provider named: a
		// credential set for nothing at all is a mistake to report, not a
		// setting to drop on the floor.
		var orphans []string
		for variable := range env {
			if strings.HasPrefix(variable, dnsPrefix) {
				orphans = append(orphans, variable)
			}
		}
		sort.Strings(orphans)
		return nil, fmt.Errorf("%s: %s configures a DNS provider, but no provider is declared; set %sPROVIDER",
			errPrefix, strings.Join(orphans, ", "), dnsPrefix)
	}

	// Longest name first, so a provider named A_B claims A_B_FIELD before a
	// provider named A would claim it as field B_FIELD.
	names := make([]string, 0, len(namedModules))
	for name := range namedModules {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if len(names[i]) != len(names[j]) {
			return len(names[i]) > len(names[j])
		}
		return names[i] < names[j]
	})

	byName := map[string]*DNSProvider{}
	for _, name := range names {
		byName[name] = &DNSProvider{
			Name:   fleetName(name),
			Module: namedModules[name],
			Fields: map[string]string{},
		}
	}
	var fallback *DNSProvider
	if defaultModule != "" {
		fallback = &DNSProvider{Module: defaultModule, Fields: map[string]string{}}
	}

	// Second pass: every remaining variable is a field or a zone list.
	for variable, value := range env {
		if !strings.HasPrefix(variable, dnsPrefix) {
			continue
		}
		rest := strings.TrimPrefix(variable, dnsPrefix)
		if rest == "PROVIDER" || strings.HasSuffix(rest, "_PROVIDER") {
			continue
		}

		target := fallback
		field := rest
		for _, name := range names {
			if after, found := strings.CutPrefix(rest, name+"_"); found && after != "" {
				target = byName[name]
				field = after
				break
			}
		}
		if target == nil {
			return nil, fmt.Errorf("%s: %s configures a DNS provider, but no provider is declared; set %sPROVIDER", errPrefix, variable, dnsPrefix)
		}
		if field == "ZONES" {
			target.Zones = splitList(value)
			continue
		}
		target.Fields[strings.ToLower(field)] = value
	}

	providers := make([]DNSProvider, 0, len(names)+1)
	for _, name := range names {
		providers = append(providers, *byName[name])
	}
	// Reversed back into the operator's reading order: shortest name first is
	// only the parser's concern.
	sort.Slice(providers, func(i, j int) bool { return providers[i].Name < providers[j].Name })
	if fallback != nil {
		providers = append(providers, *fallback)
	}
	return providers, nil
}

// splitList reads a comma-separated environment list. An environment variable
// cannot repeat, so a list in one is comma-separated -- the same rule scafo's
// own CLI uses.
func splitList(value string) []string {
	var list []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			list = append(list, item)
		}
	}
	return list
}
