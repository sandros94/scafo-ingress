# scafo-ingress

The Caddy modules scafo relies on, in one repository and one binary.

One Caddy stands in front of one or more **scafo fleets** — a fleet being one
scafo installation: one node daemon endpoint, one bearer token, one routing
document, one bucket. An agency running a fleet per client on the same machines
and a single operator running one fleet are the same shape and the same code
path.

Everything is configured in one place, the `scafo` **app**, and every module
reads it:

| Module | Caddy module ID | What it does |
| --- | --- | --- |
| The app | `scafo` | Where the fleets are, which token opens them, which DNS providers exist |
| TLS storage | `caddy.storage.scafo` | Keeps certificates in each fleet's own bucket, routed per name |
| Dynamic upstreams | `http.reverse_proxy.upstreams.scafo` | Resolves a request's backends across every fleet |
| On-demand permission | `tls.permission.scafo` | Decides, at handshake time, which names may get a certificate |
| Plain-HTTP matcher | `http.matchers.scafo_plain_http` | Matches a host whose fleet asked not to be redirected to HTTPS |

- Module path: `github.com/sandros94/scafo-ingress`
- Wire contract: [Protocol v1](PROTOCOL.md) · Maintenance: [CONTRIBUTING.md](CONTRIBUTING.md)
- Worked example: [examples/Caddyfile](examples/Caddyfile) · What the image ships: [image/Caddyfile](image/Caddyfile)
- License: MIT

## The trust model

Cloud object storage credentials are broad: a key that can write
`certificates/` can usually read everything else in the bucket too, and a TLS
terminator is the last process those credentials should live in. So the roles
split: each **scafo node daemon** holds its own bucket credentials, performs the
reads and writes, and exposes a narrow HTTP API; **Caddy** holds an endpoint URL
and a bearer token per fleet, authorizing exactly that API and nothing else.
There is no path from a compromised Caddy to the rest of any bucket, and
rotating a token never involves the cloud provider.

With more than one fleet the split goes one step further: a certificate never
leaves the fleet that owns its name. Fleet A's private key is written through
fleet A's node with fleet A's token, and a key nobody owns is written nowhere at
all. See [Certificates](#certificates).

The routing side splits the same way and for the same reason. Each daemon knows
which deployment runs on which node and on which port; Caddy is told none of it
and cannot enumerate it. It asks one read-only endpoint per fleet, with that
fleet's token, and gets back the hosts it is allowed to serve and the addresses
to serve them from — including the rule for which paths on a platform's own
domain it must refuse rather than forward.

## Quick start: environment only

**No Caddyfile to upload and no volume to bind.** The published image ships a
default Caddyfile that declares an empty `scafo` block, and the app fills itself
in from the process environment. This is the intended way to run it.

### One fleet

```sh
docker run -p 80:80 -p 443:443 -p 443:443/udp \
  -e SCAFO_FLEET_ENDPOINT=http://127.0.0.1:7400 \
  -e SCAFO_FLEET_TOKEN=... \
  ghcr.io/sandros94/scafo-caddy:latest
```

That is the whole configuration. The fleet is named `default`, its routing
document is polled every five seconds, certificates go into its bucket, and
names it routes exactly get certificates on demand.

### Several fleets

Each fleet gets a name in the variable: `SCAFO_FLEET_<NAME>_ENDPOINT`, where
`<NAME>` is `[A-Z0-9_]+` and becomes the fleet's name lowercased with
underscores turned into hyphens — `SCAFO_FLEET_CLIENT_A_ENDPOINT` is fleet
`client-a`.

```sh
docker run -p 80:80 -p 443:443 -p 443:443/udp \
  -e SCAFO_FLEET_CLIENT_A_ENDPOINT=http://10.0.0.1:7400 \
  -e SCAFO_FLEET_CLIENT_A_TOKEN=... \
  -e SCAFO_FLEET_CLIENT_B_ENDPOINT=http://10.0.0.2:7400 \
  -e SCAFO_FLEET_CLIENT_B_TOKEN=... \
  -e SCAFO_FLEET_CLIENT_B_HTTP_REDIRECT=off \
  ghcr.io/sandros94/scafo-caddy:latest
```

**The two forms cannot be mixed.** `SCAFO_FLEET_TOKEN` beside
`SCAFO_FLEET_A_TOKEN` is a provisioning error that names both variables, and so
is `SCAFO_FLEET_ENDPOINT` beside `SCAFO_FLEET_DEFAULT_ENDPOINT` — those two
would be the same fleet twice, and guessing which one was meant is exactly what
this must not do.

Nothing else is guessed either. A `SCAFO_FLEET_*` variable this build does not
recognize is an error naming it and listing the ones it does, because a
mis-spelled variable that is quietly ignored is a fleet that quietly does not
exist. A fleet with an endpoint and no token is an error too: it would fail
every fetch with a 401 and look like a daemon that is down.

### Wildcard certificates, with a DNS provider

```sh
  -e SCAFO_DNS_PROVIDER=cloudflare \
  -e SCAFO_DNS_API_TOKEN=... \
  -e SCAFO_DNS_ZONES=apps.example.com \
  -e SCAFO_ACME_DELEGATION=acme.example.com \
  -e SCAFO_ACME_EMAIL=ops@example.com
```

`SCAFO_DNS_<FIELD>` lowercased is the provider module's own JSON field, so
`SCAFO_DNS_API_TOKEN` is `api_token`. `ZONES` is reserved for this app and
cannot be a provider field name. This needs the
[`-dns` flavour](#two-flavours), which is the only build with DNS provider
modules compiled in. See [Wildcard certificates](#wildcard-certificates).

Zones spread over several providers get a name each:

```sh
  -e SCAFO_DNS_PROVIDER=cloudflare \
  -e SCAFO_DNS_API_TOKEN=... \
  -e SCAFO_DNS_BN_PROVIDER=bunny \
  -e SCAFO_DNS_BN_ZONES=other.example,third.example \
  -e SCAFO_DNS_BN_API_KEY=...
```

A base is solved by the named provider whose zone is its longest suffix, and by
the default provider otherwise. Declaring a name is what carves a field
namespace out of the default one: without `SCAFO_DNS_BN_PROVIDER`, the parser
has no way to tell `SCAFO_DNS_BN_API_KEY` (provider `bn`, field `api_key`) from
a default provider with a field called `bn_api_key`.

## Install

```sh
xcaddy build --with github.com/sandros94/scafo-ingress            # every module here
xcaddy build --with github.com/sandros94/scafo-ingress/storage    # the storage one and the app
xcaddy build --with github.com/sandros94/scafo-ingress/ingress    # the HTTP ones and the app
xcaddy build --with github.com/sandros94/scafo-ingress/permission # the permission one and the app
```

The repository root imports every module in it, so naming the root compiles them
all in; naming a single package compiles that module and the `scafo` app it
reads, and nothing else.

Or use the provided `Dockerfile`, which does the same inside the official
`caddy:<version>-builder` image and copies the result into the matching runtime
image. Its `CADDY_VERSION` is pinned exactly, and overriding it builds against
any other release:

```sh
docker build -t my-caddy .
docker build -t my-caddy --build-arg CADDY_VERSION=2.10.0 .
```

Prebuilt images and binaries are published for `linux/amd64` and `linux/arm64`,
in two flavours.

### Two flavours

**Lean** is Caddy with only the modules above. **`-dns`** is the same build with
a curated set of DNS-01 provider plugins compiled in, for the
[wildcard certificates](#wildcard-certificates) — the ACME DNS challenge needs a
plugin for the zone's provider, and a Caddy plugin can only arrive at compile
time.

| | Image | Release binaries |
| --- | --- | --- |
| Lean | `:latest`, `:X.Y.Z`, `:edge` | `scafo-caddy-linux-{amd64,arm64}` |
| With providers | `:latest-dns`, `:X.Y.Z-dns`, `:edge-dns` | `scafo-caddy-dns-linux-{amd64,arm64}` |

Each binary ships a `.sha256` alongside it and is the same binary that is inside
the image of its flavour, lifted out of that build's builder stage. Every
release also carries one `caddy-version` file naming the Caddy both flavours
embed.

```sh
docker pull ghcr.io/sandros94/scafo-caddy:latest-dns
caddy list-modules | grep -E 'scafo|dns.providers'
```

The providers are Bunny, Cloudflare, DigitalOcean, Hetzner, OVH and Route 53 —
the [caddy-dns](https://github.com/caddy-dns) module for each, pinned to an
exact version in [`dns-providers.txt`](dns-providers.txt). That file is the
whole list, and moving a pin is a reviewable commit; see
[CONTRIBUTING.md](CONTRIBUTING.md).

**Take the lean flavour unless you want managed wildcards.** HTTP-01 needs no
plugin, and neither does the on-demand TLS that covers names a fleet routes
exactly. The providers exist for the wildcard.

Naming a DNS provider module this build does not carry is a provisioning error
that lists the ones it does — usually the answer is "you wanted the `-dns`
image".

### A provider that is not in the list

The build takes any Go module the same way the curated ones arrive. With
`xcaddy` directly:

```sh
xcaddy build \
  --with github.com/sandros94/scafo-ingress \
  --with github.com/caddy-dns/gandi@v1.0.3
```

Or through this repository's `Dockerfile`, whose `DNS_PROVIDERS` build argument
takes a whitespace-separated list of pinned module paths and is exactly what the
publishing workflow passes:

```sh
# one provider of your own, and nothing else
docker build -t my-caddy --build-arg DNS_PROVIDERS=github.com/caddy-dns/gandi@v1.0.3 .

# the curated set, reproducing the published -dns image
docker build -t my-caddy --build-arg DNS_PROVIDERS="$(.github/scripts/dns-providers.sh)" .
```

One Dockerfile and one xcaddy invocation stand behind both published flavours,
so a custom build is the published build with a different argument rather than a
recipe of its own.

## The Caddyfile form

Writing a fleet down in a Caddyfile is the alternative to the environment, not a
supplement to it: **when the configuration declares at least one fleet, the
environment is not consulted for fleets at all.** The two can never be
half-merged. The same rule applies to DNS providers on their own.

`scafo` is a global option, so it goes in the leading braces block:

```caddyfile
{
	scafo {
		fleet client-a {
			endpoint http://10.0.0.1:7400
			token    {env.TOKEN_A}
		}
		fleet client-b {
			endpoint      http://10.0.0.2:7400
			token_file    /run/secrets/client-b
			http_redirect off
		}

		dns cloudflare {
			api_token {env.CLOUDFLARE_API_TOKEN}
		}

		acme_delegation acme.example.com
		acme_email      ops@example.com
	}
}
```

A fleet's name is a slug: lowercase alphanumerics separated by single hyphens.

| `fleet` option | Description |
| --- | --- |
| `endpoint` | Base URL of that fleet's node daemon API, for example `http://127.0.0.1:7400`. A path prefix is allowed and is preserved. Must use `http` or `https`. **Required.** |
| `token` | The bearer token, inline. Trimmed of surrounding whitespace. |
| `token_file` | Path to a file containing the bearer token. Read once at startup and trimmed, so a trailing newline is fine. |
| `prefer` | Whose upstreams should head every route. `self`, the default, means the node answering the request. Any other value is a node id, sent through verbatim. `none` asks for nothing — it is read here and not sent, and the daemon then leaves the fleet's own order alone. |
| `poll_interval` | How long to wait between fetches of this fleet's routing document. Default `5s`, must be positive. |
| `timeout` | Bounds one fetch of the routing document. Default `10s`, must be positive. |
| `storage_timeout` | Bounds one storage request to this fleet's daemon. Default `30s`, must be positive. It does not bound a lock acquisition, which blocks until the lock is granted or the caller's context is done. |
| `http_redirect` | `on` (the default) or `off`. Off serves this fleet's hosts over plain HTTP as well, and leaves every other fleet alone. |

**Exactly one of `token` and `token_file`.** Setting both is a configuration
error, and so is setting neither; both fail at startup, naming both options.
Once resolved the two are indistinguishable — there is one token string, and
nothing downstream knows where it came from.

| `scafo` option | Description |
| --- | --- |
| `fleet <name> { … }` | One fleet. Repeatable. |
| `dns <module> { … }` | The default DNS provider. Every option in the block is one of the provider module's own JSON fields, except `zones`. |
| `dns <name> <module> { … }` | A named DNS provider, with `zones <zone…>` saying which zones it holds. The name is a slug, like a fleet's, and is not the module name — an operator may hold two accounts at one provider. |
| `acme_delegation <fqdn>` | The fixed name a delegated DNS challenge record is written at. See [Wildcard certificates](#wildcard-certificates). |
| `acme_email <email>` | The contact address registered with the ACME CA for the managed wildcards. |
| `acme_ca <url>` | The ACME directory URL for the managed wildcards. Unset uses Caddy's default. It exists so a test can point at a local CA. |

The DNS block mirrors the environment form exactly — an operator who has read
one has read the other — and every field value is a string:

```caddyfile
dns bunny {
	access_key {env.BUNNY_API_KEY}
}

dns cf cloudflare {
	zones     a.example b.example
	api_token {env.CLOUDFLARE_API_TOKEN}
}
```

Every value accepts Caddy's global placeholders, most usefully `{env.VAR}`. The
app is provisioned before it serves anything, so request-scoped placeholders are
not available here — only environment and system values.

Note the contrast with the Caddyfile's *other* environment form,
`{$SCAFO_FLEET_TOKEN}`: that one is substituted before parsing and bakes the
secret into the adapted JSON. Use `{env.…}`, not `{$…}`.

### The modules that read the app

None of them takes an endpoint or a token any more. **The old per-directive
`endpoint`/`token` form is gone and there is no compatibility shim** — the
project is pre-public, so it was removed rather than deprecated.

```caddyfile
{
	scafo { … }

	# The TLS asset store. No options.
	storage scafo

	# Who may get a certificate on demand. No options.
	on_demand_tls {
		permission scafo
	}
}

example.com {
	reverse_proxy {
		# Where this request goes. No options.
		dynamic scafo
		lb_policy first
	}
}
```

Any option inside `storage scafo` or `dynamic scafo` is an error that says where
fleets are configured instead. `prefer`, `poll_interval` and the timeouts moved
under `fleet`, so two site blocks can no longer disagree about a fleet.

In JSON:

```json
{
  "apps": {
    "scafo": {
      "fleets": [
        { "name": "client-a", "endpoint": "http://10.0.0.1:7400", "token": "{env.TOKEN_A}" }
      ]
    }
  },
  "storage": { "module": "scafo" }
}
```

## Routing across fleets

Every fleet gets its own poller and its own cached routing document; a request
is resolved against all of them at once. A request's host, lowercased and with
any port removed, is tried in this order, and the first match answers:

1. a route naming it exactly, in any fleet;
2. a fleet API host naming it exactly;
3. a wildcard route covering it — the host with its first label replaced by `*`,
   looked up once.

So **an exact route beats a wildcard across fleets**, just as it does within
one. When two fleets publish the same host, the one that published it **first**
keeps it for as long as it goes on publishing it, and the conflict is logged
once per change rather than once per poll. A host therefore never flaps between
two fleets; the conflict is a thing to fix, not a thing to watch oscillate. Two
fleets claiming a host in the very same refresh have no "first", so the order
they were configured in decides — declaration order in a Caddyfile, and
alphabetical by name when they came from the environment.

**A fleet whose node is unreachable affects only its own hosts.** Its last
document keeps answering, and the other fleets neither know nor care. See
[Several fleets at once](PROTOCOL.md#several-fleets-at-once).

Each fleet's document is fetched again on its own interval, but a fetch that
returns the same bytes as the last one is dropped rather than republished — the
cross-fleet index is rebuilt, and a conflict logged, only when something
actually changed.

## Certificates

### Storage: a certificate never crosses a fleet

One Caddy holds one CertMagic and therefore one keyspace, and every key in it
that names a domain is routed to the fleet that owns that domain — through that
fleet's node and that fleet's token. Ownership is the same resolution
[routing](#routing-across-fleets) uses, plus one rule for wildcards: a
`*.base` certificate belongs to the fleet whose document carries `*.base` as a
route, asks for `base` in its `wildcards`, or answers on `base` as its platform
apex.

The key shapes are CertMagic's own, read from its source at the version in
`go.mod`:

| Key | Belongs to |
| --- | --- |
| `certificates/<issuer>/<domain>/<domain>.{crt,key,json}` | the fleet owning `<domain>` |
| `ocsp/<domain>-<hash>` | the fleet owning `<domain>` |
| `acme/<issuer>/challenge_tokens/<domain>.json` | the fleet owning `<domain>` |
| lock `issue_cert_<domain>` | the fleet owning `<domain>` |
| `acme/<issuer>/users/…` — the ACME account | the ingress's local disk |
| `last_clean.json`, `rw_test_<n>` | the ingress's local disk |
| locks `storage_clean`, `register_acme_account…`, `ari_<id>` | the ingress's local disk |
| Caddy's own `pki/…`, `ech/…`, anything else | the ingress's local disk |

A wildcard's keys are found here too: CertMagic sanitizes `*.example.com` into
the key component `wildcard_.example.com`, and that is read back as the wildcard
it was.

**Keys that name no domain have no fleet to belong to**, so they live on the
ingress's own disk — Caddy's default file storage, under its data directory,
which is exactly where they would be with no storage module configured at all.
The ACME account is the important one: it is the ingress's, not any fleet's, and
putting it in one fleet's bucket would make that fleet able to impersonate the
ingress to its CA.

Two consequences follow from that, and both are deliberate:

- CertMagic's storage check writes and reads back a `rw_test_<random>` key
  before an issuance. That key names no domain, so it checks the **local disk**
  rather than a fleet's bucket. A fleet's bucket is exercised by the certificate
  writes themselves.
- The renewal lock CertMagic takes while fetching ARI is named after a
  certificate's ARI identifier, which carries no domain at all. It cannot be
  routed to a fleet, so it is taken locally — which makes it a lock between the
  goroutines of *this* ingress rather than between several of them. That is what
  it already was for anyone running a single Caddy.

**A domain nobody owns gets no default fleet.** Falling back to "the first one"
would put one customer's private key in another customer's bucket. So a read of
such a key is reported as missing — an error wrapping `io/fs.ErrNotExist`, which
is how CertMagic tells "absent" from "broken" — and a write is refused with an
error naming the domain.

`List` follows the same rule in both directions. A prefix that names a domain is
listed in that fleet alone. A prefix whose *children* name domains
(`certificates/<issuer>`, `ocsp`) is listed everywhere and merged, because
CertMagic's maintenance walks those to find what to renew and what to delete and
a listing that showed one fleet's keys would quietly stop maintaining the
others'. The merge filters: a key that names a domain is kept only if it came
back from the store that owns that domain, so a stale key left behind in one
fleet's bucket for a name another fleet owns never enters the listing and is
never loaded, renewed or deleted through the wrong node.

### On-demand: exact names only, and that is a choice

`tls.permission.scafo` approves a name only if some fleet's **current** routing
document names it as an exact route host or as a fleet API host. It answers from
the documents already in memory, so a handshake costs a map lookup and no HTTP
call — which matters, because this runs on the handshake path of every name the
ingress has no certificate for, and an unauthenticated stranger decides how
often that is.

**A name covered only by a `*.` entry is refused.** This is the design choice
worth stating plainly: an on-demand certificate per arbitrary subdomain is a
door anyone can knock on. Point a thousand names at this ingress and each
handshake asks the CA for another certificate under a customer's domain, until
that customer's weekly issuance limit is gone. The wildcard those names are
covered by is obtained **once**, through DNS-01, and covers all of them for
free — so the wildcard is managed and the on-demand door stays shut.

A name in no document is refused, and so is every name while no document has
arrived yet: "I do not know" is not "yes", and a cold start must not become an
issuance for whatever happens to connect first.

### Wildcard certificates

A fleet's routing document may carry a top-level `wildcards` member: the bases
it wants a `*.<base>` certificate for — its platform domains and every custom
domain claimed together with its subdomains. It is optional, and a node that
predates it simply publishes none; then nothing is managed for that fleet and
its names are covered on demand exactly as before.

For each base, **when a DNS provider is configured**, the app asks Caddy's TLS
app to manage `*.<base>` over the ACME DNS-01 challenge — plus the bare `<base>`
when that fleet's `api.hosts` carries it, because `*.example.com` does not cover
`example.com` and the platform answers on both. With no DNS provider configured
nothing is managed at all and the behaviour is on-demand-for-exact-hosts and
nothing else.

Which provider solves a base:

- the named provider whose declared `zones` entry is the base's longest suffix,
  matched at a label boundary (`example.com` covers `example.com` and
  `shop.example.com`, never `notexample.com`);
- otherwise the default provider, the one with no `zones` of its own;
- otherwise, if zones are declared somewhere and an `acme_delegation` is set,
  the **delegation**.

When no zone is declared anywhere, the default provider is the operator's only
statement of where their DNS lives, so it takes everything and the delegation
stays out of the way. **Listing zones is what turns the delegation on for the
names that are not yours.**

#### The two records a customer creates

A customer pointing their own domain at the platform creates two DNS records in
their own zone, once:

```
shop.customer.example.        A      203.0.113.10     # or a CNAME to the ingress
_acme-challenge.shop.customer.example.  CNAME  acme.example.com.
```

The first is the ordinary one that makes their name reach this ingress. The
second is the delegation: with `acme_delegation acme.example.com` (or
`SCAFO_ACME_DELEGATION`), every DNS challenge for that base is written at that
**one fixed name** in the operator's own zone, and the customer's zone is never
written to. One API credential, one record per customer, no access to anybody's
DNS but your own.

#### What this does not do yet

Automation policies are installed **once**, while Caddy is starting and nothing
is serving. Caddy reads its automation policy list on handshake paths without a
lock, so adding to it under a running server would be a data race, whatever it
bought. What a policy covers is therefore fixed at that moment.

A policy covers a zone by spelling out one pattern per label depth, because
CertMagic's `MatchWildcard` replaces a subject's labels with `*` one at a time
from the left, cumulatively: `*.example.com` as a policy subject matches
`a.example.com` and `*.example.com` and nothing deeper, and there is no pattern
at all that means "anything under this zone". A zone therefore contributes
`example.com`, `*.example.com`, `*.*.example.com` and so on to a depth of four,
which covers `*.a.b.c.d.<zone>`.

So:

- A base **under a declared `zones` entry** is picked up whenever it appears,
  with no reload. This is the case to aim for.
- A base known **at startup** works in every case, delegated ones included,
  because its exact subject goes into the policy then.
- A base that appears **after startup** and is under no declared zone — a
  delegated customer domain, or any base when the default provider lists no
  zones — is **not managed**. It is logged once per state change, naming the
  base, the reason, the fix, and the `_acme-challenge` record the customer
  needs; and it is retried on a backoff, so it starts working as soon as the
  reason goes away.

**Listing your zones is what makes later-appearing bases work without a
reload.** For delegated customer domains, a Caddy reload is what picks up ones
that appeared since it started.

A base that no provider and no delegation can solve at all is reported the same
way, once, rather than retried silently forever.

## HTTP to HTTPS

Redirection is on by default. A fleet configured with `http_redirect off` has
**its** hosts served over plain HTTP instead, and every other fleet behind the
same Caddy keeps the redirect.

Caddy decides that per site block, from the host names written in the
configuration — and this ingress has no host names written in its configuration
at all: it serves whatever the routing documents currently say, and the answer
changes without a reload. There is nothing for automatic HTTPS to enumerate. So
the decision moves to where Caddy already lets a module decide things per
request, a matcher:

```caddyfile
http:// {
	@plain scafo_plain_http
	handle @plain {
		reverse_proxy {
			dynamic scafo
		}
	}
	handle {
		redir https://{host}{uri}
	}
}
```

A host no fleet owns does not match, so an unknown name is redirected like
everything else: the default is the redirect, and only a fleet that asked for
plain HTTP gets it. The ACME HTTP challenge is unaffected either way — Caddy
answers it in the server itself, before any route or matcher runs.

## Unroutable requests

Caddy's answer to *no upstreams* is `503`, and the dynamic upstreams module
leaves it that way — a status code is a policy, and encoding one person's policy
in a module option is how modules grow options nobody else wants. What it does
instead is record why, in four request variables, and leave the answer to a
`handle_errors` route:

| Variable | Value |
| --- | --- |
| `scafo.route` | `ok`, `denied`, `unknown` or `unavailable` |
| `scafo.host` | the normalized host the lookup was keyed by |
| `scafo.matched` | the document key that answered: a route host, a wildcard such as `*.example.com`, or a fleet API host — empty when nothing matched |
| `scafo.fleet` | the name of the fleet that answered — empty when none did |

`scafo.host` is always the request's own host, whatever answered it;
`scafo.matched` is the other half, and is what tells a wildcard's traffic apart
from a route named for one deployment. `scafo.fleet` is what tells one
installation's traffic from another's.

These are **request** variables, set by the dynamic upstreams module before it
answers. A `vars` matcher branches on them, and any handler that expands
request placeholders — `respond`, `header`, `rewrite` — can read
`{http.vars.scafo.fleet}`. Caddy's access-log encoder is not one of those: it
does not run in a request context, so a fleet name reaches the log only by way
of something that does, such as a response header the log already records.

- `ok` — some fleet's document named upstreams for this request. A `503` with
  `ok` means they were all unreachable.
- `unknown` — no fleet serves this host.
- `denied` — the host is a fleet API host and the path is one the ingress must
  answer itself rather than proxy. See
  [Denied paths](PROTOCOL.md#denied-paths); this is the security rule of that
  surface, and it is enforced whatever the error route says.
- `unavailable` — **not one** fleet has a routing document yet, so nothing is
  known about this host either way. Deliberately not `unknown`: a cold start is
  not a missing deployment. Once any fleet has answered, a host nobody names is
  `unknown`.

Turning the first two of those into a `404` takes built-in directives only:

```caddyfile
handle_errors {
	@missing vars scafo.route unknown denied
	handle @missing {
		respond "No deployment answers to {http.vars.scafo.host}." 404
	}
	respond "{err.status_code} {err.status_text}" {err.status_code}
}
```

`denied` is answered the same way as `unknown` on purpose: a distinct status
would confirm that the path exists.

### Load balancing

Each daemon returns its routes' upstreams **in priority order, never as a
weighting**: the first one is where the request goes, and the rest exist for
when it fails. `lb_policy first` is what takes that at its word — with
`prefer self`, the head of the list is the process on this very node, so a
request that arrives here stays here whenever it can and leaves only when the
local copy is unhealthy.

That only means something if failure is detected, which is what the rest is for:

```caddyfile
reverse_proxy {
	dynamic scafo

	lb_policy       first
	lb_retries      2
	lb_try_duration 5s

	fail_duration 30s
	max_fails     1
}
```

`fail_duration` and `max_fails` are Caddy's passive health checks: an upstream
that fails `max_fails` times inside `fail_duration` is skipped while that window
lasts, so `first` moves down the list instead of hammering a dead process.
`max_fails` must stay at 1 here, and the reason is `first` itself: it picks the
first upstream that is not yet marked down, and a failed dial does not mark one
down until it has failed `max_fails` times — so with a higher value every retry
of the same request goes back to the same dead process, and the request fails
before the mark is ever set. One failure is enough evidence to move on, and
`fail_duration` is what lets the process back in once it is up again.

That budget is not free when nothing is reachable, because a request to an
unknown host takes the same "no upstreams" path as a route whose backends are
all down. Sizing either one generously buys nothing here: a dynamic upstream
that resolved to nothing will resolve to nothing again on the next attempt. Keep
the retries small.

## Reference: every environment variable

`<NAME>` is `[A-Z0-9_]+`, and becomes a fleet or provider name lowercased with
underscores turned into hyphens. The single and the named fleet forms are
mutually exclusive.

### Fleets

| Variable | Description |
| --- | --- |
| `SCAFO_FLEET_ENDPOINT` | Node daemon base URL of the single fleet, which is named `default`. |
| `SCAFO_FLEET_TOKEN` | Its bearer token, inline. |
| `SCAFO_FLEET_TOKEN_FILE` | Its bearer token, as a path. Exactly one of the two. |
| `SCAFO_FLEET_PREFER` | `self` (default), a node id, or `none`. |
| `SCAFO_FLEET_POLL_INTERVAL` | Between fetches of the routing document. Default `5s`. |
| `SCAFO_FLEET_TIMEOUT` | Bounds one fetch. Default `10s`. |
| `SCAFO_FLEET_STORAGE_TIMEOUT` | Bounds one storage request. Default `30s`. |
| `SCAFO_FLEET_HTTP_REDIRECT` | `on` (default) or `off`. Also accepts `true`/`false`, `yes`/`no`, `1`/`0`. |
| `SCAFO_FLEET_<NAME>_…` | The same eight, for a fleet called `<NAME>`. |

Durations are Caddy durations (`5s`, `1m30s`) and must be positive; `0s` is an
error rather than "use the default", because unlike an omitted value it cannot
be read as one.

### DNS providers

| Variable | Description |
| --- | --- |
| `SCAFO_DNS_PROVIDER` | The default provider's Caddy module name, as `caddy list-modules` prints it minus the `dns.providers.` prefix. |
| `SCAFO_DNS_ZONES` | Comma-separated zones the default provider holds. Optional; listing them is what enables the delegation and what makes later-appearing bases work without a reload. |
| `SCAFO_DNS_<FIELD>` | One of the provider module's own JSON fields, lowercased: `SCAFO_DNS_API_TOKEN` is `api_token`. |
| `SCAFO_DNS_<NAME>_PROVIDER` | A named provider's module. Declaring this is what makes `<NAME>` a namespace. |
| `SCAFO_DNS_<NAME>_ZONES` | Comma-separated zones that provider holds. |
| `SCAFO_DNS_<NAME>_<FIELD>` | One of that provider's own JSON fields. |

`ZONES` is reserved and cannot be a provider field name. Field values are
strings, which is what an environment variable can carry; the Caddyfile form
works the same way. A `SCAFO_DNS_*` variable with no provider declared anywhere
is an error naming every one of them.

### ACME and the image

| Variable | Description |
| --- | --- |
| `SCAFO_ACME_DELEGATION` | The fixed name a delegated DNS challenge record is written at. |
| `SCAFO_ACME_EMAIL` | The contact address registered with the ACME CA. Optional. |
| `SCAFO_ACME_CA` | The ACME directory URL. Optional; unset uses Caddy's default. |
| `SCAFO_ACCESS_LOG` | Read by the image's default Caddyfile only: the access log file path. Default `/dev/stdout`. The log is JSON and the file mode is `0644`. |

A value written in the configuration wins over the environment for
`acme_delegation`, `acme_email` and `acme_ca`; the environment fills in only
what the configuration left empty. Fleets and DNS providers are not merged that
way — see [The Caddyfile form](#the-caddyfile-form).

## Recipes

### Docker Compose, one fleet

The node daemon and the ingress share a network namespace, which is what makes
the endpoint a real loopback address:

```yaml
services:
  scafo-node:
    image: ghcr.io/sandros94/scafo:latest
    environment:
      SCAFO_INGRESS_TOKEN: ${INGRESS_TOKEN:?set INGRESS_TOKEN in .env}
      # ...the daemon's own bucket credentials and settings
    ports:
      - "80:80"
      - "443:443"
      - "443:443/udp"
    volumes:
      - state:/var/lib/scafo

  caddy:
    # :latest-dns if you want managed wildcards.
    image: ghcr.io/sandros94/scafo-caddy:latest
    network_mode: "service:scafo-node"
    depends_on:
      - scafo-node
    environment:
      SCAFO_FLEET_ENDPOINT: http://127.0.0.1:7400
      SCAFO_FLEET_TOKEN: ${INGRESS_TOKEN:?set INGRESS_TOKEN in .env}
    volumes:
      - caddy-data:/data

volumes:
  state:
  caddy-data:
```

`/data` is worth keeping: it is where the ACME account and Caddy's own
housekeeping live, and losing it means registering with the CA again.

### Docker Compose, several fleets

One Caddy, one node per client, each node's API reachable on the shared network:

```yaml
services:
  caddy:
    image: ghcr.io/sandros94/scafo-caddy:latest-dns
    ports:
      - "80:80"
      - "443:443"
      - "443:443/udp"
    environment:
      SCAFO_FLEET_CLIENT_A_ENDPOINT: http://node-a:7400
      SCAFO_FLEET_CLIENT_A_TOKEN: ${TOKEN_A:?}
      SCAFO_FLEET_CLIENT_B_ENDPOINT: http://node-b:7400
      SCAFO_FLEET_CLIENT_B_TOKEN: ${TOKEN_B:?}
      SCAFO_DNS_PROVIDER: cloudflare
      SCAFO_DNS_API_TOKEN: ${CLOUDFLARE_API_TOKEN:?}
      SCAFO_DNS_ZONES: apps.example.com
      SCAFO_ACME_DELEGATION: acme.example.com
    volumes:
      - caddy-data:/data

volumes:
  caddy-data:
```

Reaching a node over a shared Docker network rather than loopback means the
token is travelling over that network: keep it private, or use `https` endpoints.

### systemd

On bare metal the file path stays first-class: the daemon writes the token
`0600` as its own user inside a `0700` directory, Caddy runs as somebody else,
and neither mode should be loosened for a surface that carries private keys.
`LoadCredential=` bridges that gap — the service manager reads the file as root
and places a copy only Caddy's UID can open in `$CREDENTIALS_DIRECTORY`:

```ini
[Unit]
After=scafo-node.service

[Service]
LoadCredential=ingress-token:/var/lib/scafo/ingress-token
Environment=SCAFO_FLEET_ENDPOINT=http://127.0.0.1:7400
ExecStart=/usr/bin/caddy run --config /etc/scafo/Caddyfile
Restart=on-failure
StartLimitIntervalSec=0
```

with `SCAFO_FLEET_TOKEN_FILE` pointing into that directory, or the Caddyfile
form:

```caddyfile
{
	scafo {
		fleet default {
			endpoint   http://127.0.0.1:7400
			token_file {env.CREDENTIALS_DIRECTORY}/ingress-token
		}
	}
	storage scafo
}
```

`After=` only means the daemon was exec'd; the token appears a moment later, and
a `LoadCredential=` whose source is missing is fatal *before* any of the unit's
own commands run — so no `ExecStartPre` wait loop can help. What carries the
first boot is `Restart=on-failure` with `StartLimitIntervalSec=0`, which keeps
systemd from giving up after five starts over a gap of seconds.

## Rotating a token

**The ordering rule: the daemon first, then Caddy.** A Caddy reload refills the
certificate cache from storage, so it must find a daemon that already accepts
the token it is about to present.

Serving is unaffected throughout: certificates are served from Caddy's in-memory
cache and routes from the last document that parsed, so even a window where
Caddy holds a token a daemon rejects stalls renewals and route updates rather
than handshakes and requests. And it stalls them for **that fleet only** — every
other fleet behind the same Caddy is untouched.

**A fleet — expand, then contract:**

1. Give the daemon both tokens, new and old — `SCAFO_INGRESS_TOKEN="new,old"`,
   comma-separated, new one first — and restart it. Both are accepted; nothing
   else has moved yet.
2. Reload each Caddy with the new token, at leisure. Reloads are graceful, not
   restarts.
3. Once every Caddy is on the new token, contract the daemon to the new one
   alone — `SCAFO_INGRESS_TOKEN="new"` — and restart it. The old token stops
   working.

**A single daemon and a single Caddy** can skip the expand step: set the new
value on both, restart the daemon, reload Caddy.

## Operational notes

A daemon must be reachable when Caddy **starts** for its fleet to start already
routing, but its absence does not keep Caddy from coming back: the first fetch
happens while the app is provisioning, and its failure is logged and outlived.
Only a configuration error fails startup — a missing, unreadable or empty token
aborts with an error naming the option, while a daemon that does not answer is a
warning in the log and nothing more.

A **running** Caddy is not affected by daemon downtime. TLS handshakes are
served from the in-memory certificate cache, requests are routed from the last
document that parsed, and only background work fails and is retried later:
renewals, OCSP staple refreshes, a certificate for a new name, a route that has
changed since the last successful poll. A Caddy that starts while every daemon
is down has an empty certificate cache and no routing document, so TLS fails and
every request is `unavailable` until one answers. That is the failure mode to
design around:

- Start the daemons before Caddy and stop them after, so Caddy never outlives
  them.
- Compose: share a node's network namespace where there is one node, and
  `depends_on` it.
- systemd: order the Caddy unit `After=` the daemon's.
- Kubernetes: run a daemon as a sidecar in the Caddy pod with a startup probe,
  so it is ready before Caddy's container begins.

A **reload** is none of those things: it is graceful, and the routing documents
survive it. Each fleet's poller is reference-counted across the swap — the
incoming configuration takes its reference before the outgoing one gives its own
back — so polling never pauses and the last document that parsed keeps answering
throughout, even when a daemon happens to be down at that moment. A poller stops
only when the last configuration using it has gone.

Several Caddy instances may share one fleet's bucket, which is how CertMagic's
clustering is meant to work. Coordination relies on that daemon's lock
endpoints, and expiring locks left behind by a crashed holder is the daemon's
responsibility — see [PROTOCOL.md](PROTOCOL.md). Note that the ARI renewal lock
is local to each ingress rather than shared; see
[Storage](#storage-a-certificate-never-crosses-a-fleet).

## What has been run

Everything below was run on this code, in Docker, on 2026-09-20. Nothing else
in this document is a claim about a live system.

`go vet ./...` and `go test -race ./...` pass. The image builds in both
flavours — `docker build .` and the same with
`--build-arg DNS_PROVIDERS="$(.github/scripts/dns-providers.sh)"` — and
`caddy list-modules` in the second one lists `scafo`, `caddy.storage.scafo`,
`http.reverse_proxy.upstreams.scafo`, `http.matchers.scafo_plain_http`,
`tls.permission.scafo` and the six DNS providers.

[`internal/e2e/run.sh`](internal/e2e/README.md) then runs the **built image**
against two scafo node stand-ins and a local ACME server: **31 checks, 0
failed**. It proved:

- the image runs on its own `/etc/caddy/Caddyfile` with **nothing but
  environment variables** — two fleets, their tokens, their poll intervals and
  `http_redirect off` for one of them;
- `http_redirect off` applies to that fleet **only**: its hosts answer `200`
  over plain HTTP while the other fleet's are `308`-redirected to the same
  name over HTTPS, and a host no fleet serves is redirected like everything
  else;
- each fleet's hosts reach that fleet's own backend, including a name covered
  by a wildcard route;
- a host **both** fleets publish is served by the one that claimed it, and the
  conflict is logged **once**, not once per poll, naming the host and both
  fleets;
- a fleet API host is proxied to its own daemon, while `/v1/storage/list` and
  `/v1/ingress/routes` on that host are answered by the ingress itself — and
  `/v1/storagex`, which only shares a prefix, is proxied;
- `scafo.fleet` carries the name of the fleet that answered;
- the on-demand permission refuses a name no fleet serves, logging
  `no fleet serves …`, and no certificate is obtained for it;
- a certificate issued on demand for one fleet's host lands in **that fleet's**
  storage and nowhere else. That fleet's node held
  `certificates/…/app.a.example/app.a.example.{crt,key,json}`; the other
  fleet's node held **nothing**; the ACME account, `last_clean.json` and
  CertMagic's `rw_test_…` storage check were in neither, on the ingress's own
  disk as intended.

**No public certificate authority was contacted at any point**, and the run
asserts that: every Caddy container is given a trust store holding either
nothing or the local CA's root alone, so a request to any other CA fails at
the handshake rather than arriving.

Not run, and therefore not claimed: the wildcard certificate automation end to
end. It is covered by the table tests in `fleets/`, including the CertMagic
subject-matching behaviour the zone patterns depend on. See
[internal/e2e/README.md](internal/e2e/README.md) for why.

## Tracking Caddy

Releases follow Caddy stable automatically, so a patch bump with no module
changes means exactly one thing: Caddy moved. See
[CONTRIBUTING.md](CONTRIBUTING.md).

## Credits

This module exists because [certmagic-s3](https://github.com/ss098/certmagic-s3)
showed how small a Caddy storage module can be — reading it is what convinced me
the daemon-backed design was worth building. Thanks to the
[Caddy](https://caddyserver.com) and
[CertMagic](https://github.com/caddyserver/certmagic) maintainers and
contributors for the interfaces that make a module like this possible at all.
