package fleets

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/caddyserver/caddy/v2"
)

// DNSProvider is one DNS-01 provider an operator has credentials for.
//
// An operator whose own zones live at one provider declares one of these with
// no Name and no Zones: it is the default, and every base that is not covered
// by a named provider goes to it. An operator whose zones are spread over
// several providers names each one and lists the zones it holds, and a base is
// solved by the provider whose zone is its longest suffix.
type DNSProvider struct {
	// Name identifies the provider in this configuration. It is a slug, and
	// it is NOT the module name: an operator may hold two accounts at one
	// provider. The default provider has no name.
	Name string `json:"name,omitempty"`

	// Module is the Caddy DNS provider module compiled into this build, as
	// `caddy list-modules` prints it minus the dns.providers. prefix --
	// "cloudflare", "bunny".
	Module string `json:"module,omitempty"`

	// Zones are the DNS zones this provider holds. A base is solved by the
	// named provider whose zone is its longest suffix.
	Zones []string `json:"zones,omitempty"`

	// Fields are the provider module's own configuration, by its JSON member
	// name: "api_token", "api_key". They are strings, because an environment
	// variable is a string; the Caddyfile form hands the tokens to the
	// provider module's own parser instead and is not limited that way.
	Fields map[string]string `json:"fields,omitempty"`

	// ConfigRaw is the provider module's configuration as JSON, already
	// carrying its "name" member. The Caddyfile form fills this in by asking
	// the provider module to parse its own block; the environment form builds
	// it from Module and Fields.
	ConfigRaw json.RawMessage `json:"config,omitempty"`
}

// resolve expands placeholders and turns Module and Fields into ConfigRaw,
// unless the Caddyfile already supplied one.
func (p *DNSProvider) resolve() error {
	repl := caddy.NewReplacer()
	p.Name = strings.TrimSpace(repl.ReplaceKnown(p.Name, ""))
	p.Module = strings.TrimSpace(repl.ReplaceKnown(p.Module, ""))
	for i, zone := range p.Zones {
		p.Zones[i] = NormalizeHost(repl.ReplaceKnown(zone, ""))
	}
	if p.Name != "" && !ValidName(p.Name) {
		return fmt.Errorf("%s: DNS provider name %q is not a slug", errPrefix, p.Name)
	}
	if p.Module == "" {
		return fmt.Errorf("%s: DNS provider %q names no module", errPrefix, p.describe())
	}
	if err := p.assertCompiledIn(); err != nil {
		return err
	}
	if len(p.ConfigRaw) > 0 {
		return nil
	}

	// The provider module's own JSON, with the inline key Caddy reads the
	// module name from. Values are strings: that is what an environment
	// variable can carry, and every DNS provider module's credential fields
	// are strings.
	config := map[string]any{"name": p.Module}
	for field, value := range p.Fields {
		config[field] = repl.ReplaceKnown(value, "")
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("%s: encoding DNS provider %s: %v", errPrefix, p.describe(), err)
	}
	p.ConfigRaw = raw
	return nil
}

// assertCompiledIn refuses a provider module this binary does not carry. A
// Caddy plugin can only arrive at compile time, so naming one that is not here
// is a build choice to correct rather than a runtime condition to survive --
// and the error lists what IS here, because the answer is usually "you wanted
// the -dns image".
func (p *DNSProvider) assertCompiledIn() error {
	id := "dns.providers." + p.Module
	if _, err := caddy.GetModule(id); err == nil {
		return nil
	}
	available := caddy.GetModules("dns.providers")
	names := make([]string, 0, len(available))
	for _, mod := range available {
		names = append(names, strings.TrimPrefix(string(mod.ID), "dns.providers."))
	}
	sort.Strings(names)
	if len(names) == 0 {
		return fmt.Errorf("%s: DNS provider module %q is not compiled into this build, and neither is any other; use the -dns image flavour", errPrefix, p.Module)
	}
	return fmt.Errorf("%s: DNS provider module %q is not compiled into this build; the ones that are: %s",
		errPrefix, p.Module, strings.Join(names, ", "))
}

// describe names a provider for an error message.
func (p *DNSProvider) describe() string {
	if p.Name == "" {
		return "(default)"
	}
	return p.Name
}

// covers reports whether base falls inside one of this provider's zones, and
// how long the matching zone is. A zone matches its own apex and everything
// under it, at a label boundary, so "example.com" covers "example.com" and
// "shop.example.com" and never "notexample.com".
func (p *DNSProvider) covers(base string) (int, bool) {
	best := 0
	for _, zone := range p.Zones {
		if zone == "" {
			continue
		}
		if base == zone || strings.HasSuffix(base, "."+zone) {
			if len(zone) > best {
				best = len(zone)
			}
		}
	}
	return best, best > 0
}

// providerFor picks the DNS provider that will solve the challenge for base.
//
// A named provider whose zone is base's longest suffix wins; otherwise the
// default provider does, if there is one. A base under nobody's zone and with
// no default provider has no provider at all, which is what the delegation is
// for -- see wildcards.go.
func (a *App) providerFor(base string) (*DNSProvider, bool) {
	var best *DNSProvider
	bestLen := 0
	for i := range a.DNS {
		provider := &a.DNS[i]
		if len(provider.Zones) == 0 {
			continue
		}
		if length, ok := provider.covers(base); ok && length > bestLen {
			best, bestLen = provider, length
		}
	}
	if best != nil {
		return best, true
	}
	for i := range a.DNS {
		if provider := &a.DNS[i]; len(provider.Zones) == 0 {
			return provider, true
		}
	}
	return nil, false
}

// hasDNS reports whether any DNS provider is configured at all. Without one
// nothing is managed and the app behaves exactly as it does today: on-demand
// certificates for exact hosts and nothing else.
func (a *App) hasDNS() bool { return len(a.DNS) > 0 }
