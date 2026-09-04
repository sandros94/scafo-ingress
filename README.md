# scafo-ingress

The Caddy modules scafo relies on, in one repository and one binary.

`caddy.storage.scafo` is a TLS storage module that keeps certificates, private
keys and ACME account data in a bucket **without giving Caddy the bucket
credentials**. It implements CertMagic's `Storage` interface (including its
`Locker`) by turning every storage operation into an HTTP call to a colocated
*scafo node daemon*. The daemon owns the bucket connection; Caddy owns nothing
but a loopback URL and a bearer token.

`http.reverse_proxy.upstreams.scafo` is a dynamic upstreams source that answers
`reverse_proxy`'s question — *where does this request go?* — from the fleet
routing document that same daemon serves. No backend address is written down in
the Caddy configuration: Caddy asks the node where a host is running right now,
so a deployment that moves between nodes costs no reload.

| Module | Caddy module ID | Package |
| --- | --- | --- |
| TLS storage | `caddy.storage.scafo` | `github.com/sandros94/scafo-ingress/storage` |
| Dynamic upstreams | `http.reverse_proxy.upstreams.scafo` | `github.com/sandros94/scafo-ingress/ingress` |

- Module path: `github.com/sandros94/scafo-ingress`
- Wire contract: [Protocol v1](PROTOCOL.md) · Maintenance: [CONTRIBUTING.md](CONTRIBUTING.md)
- Worked example: [examples/Caddyfile](examples/Caddyfile)
- License: MIT

## The trust model

Cloud object storage credentials are broad: a key that can write
`certificates/` can usually read everything else in the bucket too, and a TLS
terminator is the last process those credentials should live in. So the roles
split: the **scafo node daemon** holds the bucket credentials, performs the reads and
writes, and exposes a narrow HTTP API on loopback; **Caddy** holds an endpoint
URL and a bearer token that authorizes exactly that API and nothing else. There
is no path from a compromised Caddy to the rest of the bucket, and rotating the
token never involves the cloud provider. Everything Caddy needs is two
configuration values, neither of which is a cloud secret.

The routing side splits the same way and for the same reason. The daemon knows
which deployment runs on which node and on which port; Caddy is told none of it
and cannot enumerate it. It asks one read-only endpoint, with the same token,
and gets back the hosts it is allowed to serve and the addresses to serve them
from — including the rule for which paths on the platform's own domain it must
refuse rather than forward.

## Install

```sh
xcaddy build --with github.com/sandros94/scafo-ingress          # every module here
xcaddy build --with github.com/sandros94/scafo-ingress/storage  # only the storage one
xcaddy build --with github.com/sandros94/scafo-ingress/ingress  # only the upstreams one
```

The repository root imports every module in it, so naming the root compiles them
all in; naming a single module compiles that one and nothing else.

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

**Lean** is Caddy with only the two modules above. **`-dns`** is the same build
with a curated set of DNS-01 provider plugins compiled in, for the wildcard
certificate described [below](#a-complete-caddyfile) — the ACME DNS challenge
needs a plugin for the zone's provider, and a Caddy plugin can only arrive at
compile time.

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
# caddy.storage.scafo
# dns.providers.bunny
# dns.providers.cloudflare
# dns.providers.digitalocean
# dns.providers.hetzner
# dns.providers.ovh
# dns.providers.route53
# http.reverse_proxy.upstreams.scafo
```

The providers are Bunny, Cloudflare, DigitalOcean, Hetzner, OVH and Route 53 —
the [caddy-dns](https://github.com/caddy-dns) module for each, pinned to an
exact version in [`dns-providers.txt`](dns-providers.txt). That file is the
whole list, and moving a pin is a reviewable commit; see
[CONTRIBUTING.md](CONTRIBUTING.md).

**Take the lean flavour unless you need DNS-01.** HTTP-01 needs no plugin, and
neither does the on-demand TLS that covers customer domains. The providers exist
for the wildcard.

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

## Connecting to the daemon

Both modules take the same three options and mean the same thing by them.

| Option | Description |
| --- | --- |
| `endpoint` | Base URL of the node daemon's API, for example `http://127.0.0.1:7400`. A path prefix is allowed and is preserved. Must use `http` or `https`. **Required.** |
| `token` | The bearer token, inline. Trimmed of surrounding whitespace. |
| `token_file` | Path to a file containing the bearer token. Read once at startup and trimmed, so a trailing newline is fine. |

**Exactly one of `token` and `token_file`.** Setting both is a configuration
error, and so is setting neither; both fail at startup, naming both options.
Once provisioned the two are indistinguishable — there is one token string, and
nothing downstream knows where it came from.

Every value accepts Caddy's global placeholders, most usefully `{env.VAR}`.
Both modules are provisioned before they serve anything, so request-scoped
placeholders are not available here — only environment and system values.

**Prefer `{env.SCAFO_INGRESS_TOKEN}` to a literal token.** The placeholder is
resolved during provisioning and is never written back into the configuration.
Verified against Caddy 2.11.4: `caddy adapt` emits
`"token": "{env.SCAFO_INGRESS_TOKEN}"` verbatim, and the admin API's
`GET /config/` returns the placeholder verbatim too — so neither a config file
nor a config dump carries the token, only the running process holds it.

Note the contrast with the Caddyfile's *other* environment form,
`{$SCAFO_INGRESS_TOKEN}`: that one is substituted before parsing and does bake
the secret into the adapted JSON. Use `{env.…}`, not `{$…}`.

The token authorizes these two APIs only, and the endpoint is expected to be
loopback or an equally private network path. Do not expose a node daemon's API
publicly on the strength of the token alone.

## TLS storage — `caddy.storage.scafo`

`storage` is a global option, so it goes in the leading braces block:

```caddyfile
{
	storage scafo {
		endpoint http://127.0.0.1:7400
		token    {env.SCAFO_INGRESS_TOKEN}
	}
}

example.com {
	respond "hello"
}
```

```json
{
  "storage": {
    "module": "scafo",
    "endpoint": "http://127.0.0.1:7400",
    "token": "{env.SCAFO_INGRESS_TOKEN}"
  }
}
```

The token may instead be read from a file, which is the better fit wherever a
credential manager hands one over as a path:

```caddyfile
{
	storage scafo {
		endpoint   http://127.0.0.1:7400
		token_file /var/lib/scafo/storage-token
	}
}
```

## Dynamic upstreams — `http.reverse_proxy.upstreams.scafo`

It goes inside a `reverse_proxy` block, as `dynamic scafo`:

```caddyfile
reverse_proxy {
	dynamic scafo {
		endpoint http://127.0.0.1:7400
		token    {env.SCAFO_INGRESS_TOKEN}
	}
	lb_policy first
}
```

```json
{
  "handler": "reverse_proxy",
  "dynamic_upstreams": {
    "source": "scafo",
    "endpoint": "http://127.0.0.1:7400",
    "token": "{env.SCAFO_INGRESS_TOKEN}"
  },
  "load_balancing": { "selection_policy": { "policy": "first" } }
}
```

Besides the connection options it takes two of its own:

| Option | Description |
| --- | --- |
| `prefer` | Whose upstreams should head every route. `self`, the default, means the node answering the request. Any other value is a node id, sent through verbatim, and one that names no node in the fleet simply has no effect. `none` asks for nothing — it is read here and not sent, and the daemon then leaves the fleet's own order alone. |
| `poll_interval` | How long to wait between fetches of the routing document. Default `5s`, must be positive. It bounds how stale a route may be: how long a deployment that has just moved keeps being proxied to where it was. |

The routing document is fetched in the background and cached, so a request costs
a map lookup and no round trip to the daemon. A request's host, minus any port
and lowercased, is the lookup key; a deployment's route wins over the platform's
own API surface if they ever share a name.

**One poller, not one per site block.** A snippet imported into twenty site
blocks is twenty instances of this module, and they share a single poller and a
single cached document: everything with the same `endpoint`, token, `prefer` and
`poll_interval` is one conversation with the daemon, so the fetch happens once
per interval no matter how many blocks ask for it. Only what changes the
document, or how often it is asked for, splits them apart.

**The daemon must be reachable when Caddy starts, but not for Caddy to start.**
The first fetch is attempted during provisioning, so a healthy pair begins
already routing; if it fails, the failure is logged and the poller carries on,
because a node restarting under a running ingress must not be able to keep Caddy
from coming back. Only a configuration error fails startup. Once a document has
arrived, later fetch failures cost nothing at all: the last one that parsed
keeps answering, and the log says so once rather than once per poll.

A reload does not start over either. The incoming configuration's handlers pick
up the poller that is already running before the outgoing ones let go of it, so
the document that was answering requests keeps answering them across the swap —
including when the daemon happens to be down at that moment. The poller stops
only when the last handler using it goes away.

### Load balancing

The daemon returns each route's upstreams **in priority order, never as a
weighting**: the first one is where the request goes, and the rest exist for
when it fails. `lb_policy first` is what takes that at its word — with
`prefer self`, the head of the list is the process on this very node, so a
request that arrives here stays here whenever it can and leaves only when the
local copy is unhealthy.

That only means something if failure is detected, which is what the rest is for:

```caddyfile
reverse_proxy {
	dynamic scafo {
		endpoint http://127.0.0.1:7400
		token    {env.SCAFO_INGRESS_TOKEN}
	}

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
`lb_retries` and `lb_try_duration` bound how long one request keeps trying
before it gives up — Caddy stops at whichever of the two it reaches first, and
waits `lb_try_interval` between attempts (250ms by default, but only when
`lb_try_duration` is set at all; on its own, `lb_retries` retries immediately).

That budget is not free when nothing is reachable, because a request to an
unknown host takes the same "no upstreams" path as a route whose backends are
all down. With the values above it costs `lb_retries` × 250ms — half a second —
before the 503 becomes the 404 below. Sizing either one generously buys nothing
here: a dynamic upstream that resolved to nothing will resolve to nothing again
on the next attempt. Keep the retries small.

### Unroutable requests

Caddy's answer to *no upstreams* is `503`, and this module leaves it that way —
a status code is a policy, and encoding one person's policy in a module option
is how modules grow options nobody else wants. What it does instead is record
why, in two request variables, and leave the answer to a `handle_errors` route:

| Variable | Value |
| --- | --- |
| `scafo.route` | `ok`, `denied`, `unknown` or `unavailable` |
| `scafo.host` | the normalized host the lookup was keyed by |

- `ok` — the document named upstreams for this request. A `503` with `ok` means
  they were all unreachable.
- `unknown` — the fleet does not serve this host.
- `denied` — the host is a platform API host and the path is one the ingress
  must answer itself rather than proxy. See
  [Denied paths](PROTOCOL.md#denied-paths); this is the security rule of that
  surface, and it is enforced whatever the error route says.
- `unavailable` — no routing document has arrived yet, so nothing is known
  about this host either way. Deliberately not `unknown`: a cold start is not a
  missing deployment.

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

## A complete Caddyfile

[examples/Caddyfile](examples/Caddyfile) is the whole thing — both modules,
on-demand TLS for customer domains, a wildcard certificate for the platform
domain, and the error route above with a page rather than a sentence. In
outline:

```caddyfile
{
	storage scafo {
		endpoint http://127.0.0.1:7400
		token    {env.SCAFO_INGRESS_TOKEN}
	}

	# The daemon decides which customer domains it will issue for. This
	# endpoint is public-facing input to a private decision, so it is the
	# one scafo path Caddy calls without a token.
	on_demand_tls {
		ask http://127.0.0.1:7400/v1/ingress/ask
	}
}

(scafo) {
	reverse_proxy {
		dynamic scafo {
			endpoint http://127.0.0.1:7400
			token    {env.SCAFO_INGRESS_TOKEN}
		}
		lb_policy     first
		fail_duration 30s
	}

	handle_errors {
		@missing vars scafo.route unknown denied
		handle @missing {
			respond "No deployment answers to {http.vars.scafo.host}." 404
		}
		respond "{err.status_code} {err.status_text}" {err.status_code}
	}
}

# The platform domain and every deployment under it, on one certificate.
*.dev.example, dev.example {
	tls {
		dns cloudflare {env.CLOUDFLARE_API_TOKEN}
	}
	import scafo
}

# Customer domains pointed at this fleet.
https:// {
	tls {
		on_demand
	}
	import scafo
}
```

**The wildcard is the suggested default for the platform domain.** DNS-01 needs
no inbound request per name, so a new deployment is reachable over HTTPS the
moment the daemon starts routing it, and one certificate covers every
subdomain. It costs the [`-dns` flavour](#two-flavours) — `dns cloudflare` above
resolves to `dns.providers.cloudflare`, which is only in that build — and an API
token for the zone. Without either, drop the `tls` block: Caddy then issues per
name over HTTP-01, which is correct and runs on the lean flavour, but every new
subdomain waits for its own issuance the first time it is asked for.

The `https://` block is a catch-all for every other name that reaches the HTTPS
port, and `on_demand` hands the decision to the daemon's `ask` endpoint — the
only scafo path Caddy calls without a token, because it is the daemon's job to
decide which customer domains belong to this fleet.

## Recipes

### Docker Compose

One environment variable, shared by both services. There is no token file, and
therefore nothing to mount:

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
    # :latest-dns if the Caddyfile asks for a DNS-01 wildcard.
    image: ghcr.io/sandros94/scafo-caddy:latest
    # Sharing the node's network namespace is what makes the endpoint below a
    # real loopback address. It also means Caddy has no network of its own:
    # its ports are published from the node service above.
    network_mode: "service:scafo-node"
    depends_on:
      - scafo-node
    environment:
      SCAFO_INGRESS_TOKEN: ${INGRESS_TOKEN:?set INGRESS_TOKEN in .env}
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro

volumes:
  state:
```

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
LoadCredential=storage-token:/var/lib/scafo/storage-token
ExecStart=/usr/bin/caddy run --config /etc/scafo/caddy.json
Restart=on-failure
StartLimitIntervalSec=0
```

```json
{ "storage": { "module": "scafo", "endpoint": "http://127.0.0.1:7400",
               "token_file": "{env.CREDENTIALS_DIRECTORY}/storage-token" } }
```

`After=` only means scafod was exec'd; the token appears a moment
later, and a `LoadCredential=` whose source is missing is fatal *before* any of
the unit's own commands run — so no `ExecStartPre` wait loop can help. What
carries the first boot is `Restart=on-failure` with `StartLimitIntervalSec=0`,
which keeps systemd from giving up after five starts over a gap of seconds.

## Rotating the token

**The ordering rule: the daemon first, then Caddy.** A Caddy reload refills the
certificate cache from storage, so it must find a daemon that already accepts
the token it is about to present.

Serving is unaffected throughout, in every variant below: certificates are
served from Caddy's in-memory cache and routes from the last document that
parsed, so even a window where Caddy holds a token the daemon rejects stalls
renewals and route updates rather than handshakes and requests.

**A fleet — expand, then contract:**

1. Give the daemon both tokens, new and old — `SCAFO_INGRESS_TOKEN="new,old"`,
   comma-separated, new one first — and restart it. Both are accepted, the
   daemon's token file now holds the new one; nothing else has moved yet.
2. Reload each Caddy with the new token, at leisure — one at a time, on your own
   schedule. Reloads are graceful, not restarts.
3. Once every Caddy is on the new token, contract the daemon to the new one
   alone — `SCAFO_INGRESS_TOKEN="new"` — and restart it. The old token stops
   working.

**A single daemon and a single Caddy** can skip the expand step: set the new
value on both, restart the daemon, reload Caddy.

## Operational notes

The daemon must be reachable when Caddy **starts**. A **running** Caddy is not
affected by daemon downtime — TLS handshakes are served from the in-memory
certificate cache, requests are routed from the last document that parsed, and
only background work fails and is retried later: renewals, OCSP staple
refreshes, a certificate for a new name, a route that has changed since the last
successful poll. A Caddy that starts or restarts while the daemon is down has an
empty certificate cache and no routing document, so TLS fails and every request
is `unavailable` until the daemon returns. That is the failure mode to design
around:

- Start the daemon before Caddy and stop it after, so Caddy never outlives it.
- Compose: share the node's network namespace, as above, and `depends_on` it.
- systemd: order the Caddy unit `After=` the daemon's.
- Kubernetes: run the daemon as a sidecar in the Caddy pod with a startup probe,
  so it is ready before Caddy's container begins.

A **reload** is none of those things: it is graceful, and the routing document
survives it along with the certificate cache, because the incoming
configuration's handlers inherit the poller the outgoing ones were using. A
daemon that is down during a reload is not a window of `unavailable`.

Provisioning is deliberately loud about configuration and deliberately quiet
about the daemon: a missing, unreadable or empty token aborts startup with an
error naming the option, while a daemon that does not answer the first routing
fetch is a warning in the log and nothing more.

Several Caddy instances may share one bucket, which is how CertMagic's
clustering is meant to work. Coordination relies on the lock endpoints, and
expiring locks left behind by a crashed holder is the daemon's responsibility —
see [PROTOCOL.md](PROTOCOL.md).

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
