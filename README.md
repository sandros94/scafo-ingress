# scafo-certmagic

A Caddy TLS storage module that keeps certificates, private keys and ACME
account data in a bucket **without giving Caddy the bucket credentials**.

It implements CertMagic's `Storage` interface (including its `Locker`) by
turning every storage operation into an HTTP call to a colocated *scafo agent*.
The agent owns the bucket connection; Caddy owns nothing but a loopback URL and
a bearer token.

- Module path: `github.com/sandros94/scafo-certmagic`
- Caddy module ID: `caddy.storage.scafo`
- Wire contract: [Protocol v1](PROTOCOL.md) · Maintenance: [CONTRIBUTING.md](CONTRIBUTING.md)
- License: Apache-2.0

## The trust model

Cloud object storage credentials are broad: a key that can write
`certificates/` can usually read everything else in the bucket too, and a TLS
terminator is the last process those credentials should live in. So the roles
split: the **scafo agent** holds the bucket credentials, performs the reads and
writes, and exposes a narrow HTTP API on loopback; **Caddy** holds an endpoint
URL and a bearer token that authorizes exactly that API and nothing else. There
is no path from a compromised Caddy to the rest of the bucket, and rotating the
token never involves the cloud provider. Everything Caddy needs is two
configuration values, neither of which is a cloud secret.

## Install

```sh
xcaddy build --with github.com/sandros94/scafo-certmagic
```

Or use the provided `Dockerfile`, which does the same inside the official
`caddy:<version>-builder` image and copies the result into the matching runtime
image. Its `CADDY_VERSION` is pinned exactly, and overriding it builds against
any other release:

```sh
docker build -t my-caddy .
docker build -t my-caddy --build-arg CADDY_VERSION=2.10.0 .
```

Prebuilt images and binaries are published for `linux/amd64` and `linux/arm64`.
Every release carries `scafo-caddy-linux-{amd64,arm64}` with a `.sha256`
alongside each — the same binaries that are inside the image — and a
`caddy-version` file naming the Caddy release they embed.

```sh
docker pull ghcr.io/sandros94/scafo-caddy:latest
caddy list-modules | grep scafo     # caddy.storage.scafo
```

## Configuration

| Option | Description |
| --- | --- |
| `endpoint` | Base URL of the agent's storage API, for example `http://127.0.0.1:7400`. A path prefix is allowed and is preserved. Must use `http` or `https`. **Required.** |
| `token` | The bearer token, inline. Trimmed of surrounding whitespace. |
| `token_file` | Path to a file containing the bearer token. Read once at startup and trimmed, so a trailing newline is fine. |

**Exactly one of `token` and `token_file`.** Setting both is a configuration
error, and so is setting neither; both fail at startup, naming both options.
Once provisioned the two are indistinguishable — there is one token string, and
nothing downstream knows where it came from.

Every value accepts Caddy's global placeholders, most usefully `{env.VAR}`.
Storage is provisioned before Caddy's HTTP app starts, so request-scoped
placeholders are not available here — only environment and system values.

**Prefer `{env.SCAFO_STORAGE_TOKEN}` to a literal token.** The placeholder is
resolved when storage is provisioned and is never written back into the
configuration. Verified against Caddy 2.11.4: `caddy adapt` emits
`"token": "{env.SCAFO_STORAGE_TOKEN}"` verbatim, and the admin API's
`GET /config/` returns the placeholder verbatim too — so neither a config file
nor a config dump carries the token, only the running process holds it.

Note the contrast with the Caddyfile's *other* environment form,
`{$SCAFO_STORAGE_TOKEN}`: that one is substituted before parsing and does bake
the secret into the adapted JSON. Use `{env.…}`, not `{$…}`.

### Caddyfile

`storage` is a global option, so it goes in the leading braces block:

```caddyfile
{
	storage scafo {
		endpoint http://127.0.0.1:7400
		token    {env.SCAFO_STORAGE_TOKEN}
	}
}

example.com {
	respond "hello"
}
```

### JSON

```json
{
  "storage": {
    "module": "scafo",
    "endpoint": "http://127.0.0.1:7400",
    "token": "{env.SCAFO_STORAGE_TOKEN}"
  }
}
```

## Recipes

### Docker Compose

One environment variable, shared by both services. There is no token file, and
therefore nothing to mount:

```yaml
services:
  scafo-agent:
    image: ghcr.io/sandros94/scafo:latest
    environment:
      SCAFO_STORAGE_TOKEN: ${STORAGE_TOKEN:?set STORAGE_TOKEN in .env}
      # ...the agent's own bucket credentials and settings
    ports:
      - "80:80"
      - "443:443"
      - "443:443/udp"
    volumes:
      - state:/var/lib/scafo

  caddy:
    image: ghcr.io/sandros94/scafo-caddy:latest
    # Sharing the agent's network namespace is what makes the endpoint below a
    # real loopback address. It also means Caddy has no network of its own:
    # its ports are published from the agent service above.
    network_mode: "service:scafo-agent"
    depends_on:
      - scafo-agent
    environment:
      SCAFO_STORAGE_TOKEN: ${STORAGE_TOKEN:?set STORAGE_TOKEN in .env}
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro

volumes:
  state:
```

### systemd

On bare metal the file path stays first-class: the agent writes the token
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

`After=` only means the agent's process was exec'd; the token appears a moment
later, and a `LoadCredential=` whose source is missing is fatal *before* any of
the unit's own commands run — so no `ExecStartPre` wait loop can help. What
carries the first boot is `Restart=on-failure` with `StartLimitIntervalSec=0`,
which keeps systemd from giving up after five starts over a gap of seconds.

## Rotating the token

**The ordering rule: agent first, then Caddy.** A Caddy reload refills the
certificate cache from storage, so it must find an agent that already accepts
the token it is about to present.

Serving is unaffected throughout, in every variant below: certificates are
served from Caddy's in-memory cache, so even a window where Caddy holds a token
the agent rejects stalls renewals rather than handshakes.

**A fleet — expand, then contract:**

1. Give the agent both tokens, new and old — `SCAFO_STORAGE_TOKEN="new,old"`,
   comma-separated, new one first — and restart it. Both are accepted, the
   agent's token file now holds the new one; nothing else has moved yet.
2. Reload each Caddy with the new token, at leisure — one at a time, on your own
   schedule. Reloads are graceful, not restarts.
3. Once every Caddy is on the new token, contract the agent to the new one
   alone — `SCAFO_STORAGE_TOKEN="new"` — and restart it. The old token stops
   working.

**A single agent and a single Caddy** can skip the expand step: set the new
value on both, restart the agent, reload Caddy.

## Operational notes

The agent must be reachable when Caddy **starts**. A **running** Caddy is not
affected by agent downtime — TLS handshakes are served from the in-memory
certificate cache, and only background work that touches storage (renewals,
OCSP staple refreshes, a certificate for a new name) fails and is retried later.
A Caddy that starts, restarts, or reloads while the agent is down has an empty
certificate cache and no way to fill it, so TLS fails until the agent returns.
That is the failure mode to design around:

- Start the agent before Caddy and stop it after, so Caddy never outlives it.
- Compose: share the agent's network namespace, as above, and `depends_on` it.
- systemd: order the Caddy unit `After=` the agent's.
- Kubernetes: run the agent as a sidecar in the Caddy pod with a startup probe,
  so it is ready before Caddy's container begins.

Provisioning is deliberately loud: a missing, unreadable or empty token aborts
startup with an error naming the option, rather than deferring the problem to
the first handshake.

Several Caddy instances may share one bucket, which is how CertMagic's
clustering is meant to work. Coordination relies on the lock endpoints, and
expiring locks left behind by a crashed holder is the agent's responsibility —
see [PROTOCOL.md](PROTOCOL.md).

The token authorizes the storage API only, and the endpoint is expected to be
loopback or an equally private network path. Do not expose an agent's storage
API publicly on the strength of the token alone.

## Tracking Caddy

Releases follow Caddy stable automatically, so a patch bump with no module
changes means exactly one thing: Caddy moved. See
[CONTRIBUTING.md](CONTRIBUTING.md).

## Credits

This module exists because [certmagic-s3](https://github.com/ss098/certmagic-s3)
showed how small a Caddy storage module can be — reading it is what convinced me
the agent-backed design was worth building. Thanks to the
[Caddy](https://caddyserver.com) and
[CertMagic](https://github.com/caddyserver/certmagic) maintainers and
contributors for the interfaces that make a module like this possible at all.
