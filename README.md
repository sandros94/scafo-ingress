# scafo-certmagic

A Caddy TLS storage module that keeps certificates, private keys and ACME
account data in a bucket **without giving Caddy the bucket credentials**.

The module implements CertMagic's `Storage` interface (including its `Locker`)
by turning every storage operation into an HTTP call to a colocated *scafo
agent*. The agent owns the bucket connection; Caddy owns nothing but a loopback
URL and a bearer token.

- Module path: `github.com/sandros94/scafo-certmagic`
- Caddy module ID: `caddy.storage.scafo`
- License: Apache-2.0

## The trust model

Cloud object storage credentials are broad: an S3 key that can write
`certificates/` can usually read everything else in the bucket too. A TLS
terminator is one of the most exposed processes in a deployment, and it is the
last place those credentials should live.

This module splits the two roles:

- **The scafo agent** holds the bucket credentials, performs the actual reads
  and writes, and exposes a narrow HTTP API on loopback. It is the only process
  that can address the bucket.
- **Caddy** holds an endpoint URL and a bearer token. The token authorizes
  exactly one thing: the storage API described below. There is no path from a
  compromised Caddy to the rest of the bucket, and rotating the token does not
  involve the cloud provider at all.

Everything Caddy needs is therefore reduced to two configuration values, neither
of which is a cloud secret.

## Installation

Build a Caddy binary with the module compiled in:

```sh
xcaddy build --with github.com/sandros94/scafo-certmagic
```

Or use the provided `Dockerfile`, which does the same inside the official
`caddy:<version>-builder` image and copies the result into the matching runtime
image. The version is pinned exactly — see [Tracking Caddy](#tracking-caddy) —
and overriding it builds against any other release:

```sh
docker build -t my-caddy .
docker build -t my-caddy --build-arg CADDY_VERSION=2.10.0 .   # some other release
```

Prebuilt images and binaries are published for `linux/amd64` and `linux/arm64`:

```sh
docker pull ghcr.io/sandros94/scafo-caddy:latest
```

Every release also carries plain `scafo-caddy-linux-amd64` and
`scafo-caddy-linux-arm64` binaries with a `.sha256` alongside each, and a
`caddy-version` file naming the Caddy release they embed. They are the same
binaries that are inside the image.

Confirm the module is present:

```sh
caddy list-modules | grep scafo
# caddy.storage.scafo
```

## Configuration

Two options, both required:

| Option | Description |
| --- | --- |
| `endpoint` | Base URL of the agent's storage API, for example `http://127.0.0.1:7400`. A path prefix is allowed and is preserved. Must use `http` or `https`. |
| `token_file` | Path to a file containing the bearer token. Read once at startup; surrounding whitespace is trimmed, so a trailing newline is fine. |

Both values accept Caddy's global placeholders, most usefully `{env.VAR}`.
Storage is provisioned before Caddy's HTTP app starts, so request-scoped
placeholders are not available here — only environment and system values.

### Caddyfile

`storage` is a global option, so it goes in the leading braces block:

```caddyfile
{
	storage scafo {
		endpoint   http://127.0.0.1:7400
		token_file /var/lib/scafo/storage-token
	}
}

example.com {
	respond "hello"
}
```

With environment expansion:

```caddyfile
{
	storage scafo {
		endpoint   {env.SCAFO_STORAGE_ENDPOINT}
		token_file {env.SCAFO_STORAGE_TOKEN_FILE}
	}
}
```

### JSON

```json
{
  "storage": {
    "module": "scafo",
    "endpoint": "http://127.0.0.1:7400",
    "token_file": "/var/lib/scafo/storage-token"
  },
  "apps": {
    "http": {
      "servers": {
        "srv0": {
          "listen": [":443"],
          "routes": []
        }
      }
    }
  }
}
```

## Protocol v1

This is the canonical description of the wire protocol between this module and
the scafo agent. The base URL is the configured `endpoint`.

### Common request headers

Every request carries both:

| Header | Value |
| --- | --- |
| `Authorization` | `Bearer <token>`, the trimmed contents of `token_file` |
| `X-Scafo-Storage-Version` | `1` |

### Key encoding

Storage keys use `/` as a component separator and have no leading or trailing
slash, for example `certificates/acme-v02.api.letsencrypt.org-directory/example.com/example.com.crt`.
A key maps onto the request path as-is: each component is percent-encoded
individually and the `/` separators are preserved, so the agent sees the same
hierarchy the caller used. Query-parameter keys (`list`, `stat`) are sent with
ordinary form encoding.

### Endpoints

| Method | Path | Request | Success | Not found |
| --- | --- | --- | --- | --- |
| `PUT` | `/v1/storage/objects/<key>` | raw value bytes | `204` | — |
| `GET` | `/v1/storage/objects/<key>` | — | `200` with raw value bytes | `404` |
| `DELETE` | `/v1/storage/objects/<key>` | — | `204`; deletes the key and its subtree | `404` |
| `GET` | `/v1/storage/list?prefix=<p>&recursive=<true\|false>` | — | `200` with JSON `[]string` of keys | `404` |
| `GET` | `/v1/storage/stat?key=<k>` | — | `200` with a JSON key-info object | `404` |
| `POST` | `/v1/storage/locks/<name>` | — | `204`, lock acquired | — |
| `DELETE` | `/v1/storage/locks/<name>` | — | `204`, always, idempotent | — |

`GET /v1/storage/list` answers `404` when nothing exists at or under the
prefix — a `200` response always carries at least one key. The distinction
matters: CertMagic expects listing a missing prefix to be reported as "not
found", not as an empty result.

`POST /v1/storage/locks/<name>` additionally answers `409 Conflict` when the
lock is currently held elsewhere. The client waits one second and retries,
indefinitely, until the lock is granted or the caller's context is cancelled.
Any other error status ends the attempt immediately rather than being retried,
so a broken agent surfaces as a failure instead of an endless poll.

The stat body is:

```json
{
  "key": "certificates/acme/example.com/example.com.crt",
  "modified": "2026-04-01T12:30:00Z",
  "size": 4096,
  "isTerminal": true
}
```

`modified` is an RFC 3339 timestamp or `null` when the agent does not know it;
`null` becomes CertMagic's zero time, which it treats as "unknown". `isTerminal`
is `true` for keys that hold a value and `false` for keys that merely act as a
prefix for other keys ("directories").

### Errors

Non-2xx responses use `application/problem+json`. The `title` and `detail`
members, when present, are surfaced in the error the module returns; otherwise
the error reports the status code alone.

A `404` is translated into an error wrapping `io/fs.ErrNotExist`, which is what
CertMagic checks with `errors.Is` to distinguish a missing key from a broken
backend. Getting this wrong makes CertMagic treat absent certificates as
infrastructure failures, so agents must return `404` — not `500` — for keys that
do not exist.

### Client behaviour

The module uses a dedicated HTTP client with a 30-second per-request timeout. It
performs no retries other than the lock poll described above: the agent is
colocated, so a failure is a fault to report rather than a transient to paper
over.

## Operational notes

### Start ordering

The agent must be reachable when Caddy starts.

A **running** Caddy is not affected by agent downtime: certificates live in
Caddy's in-memory cache, so TLS handshakes continue to succeed while the agent
is unavailable. Only background work that touches storage — renewals, OCSP
staple refreshes, issuing a certificate for a new name — fails, and it is
retried later.

A Caddy that **starts or restarts** while the agent is down has an empty
certificate cache and no way to fill it, so TLS fails until the agent returns.
This includes config reloads that replace the certificate cache, and it is the
failure mode to design around:

- Start the agent before Caddy and stop it after, so Caddy never outlives it.
- Under Docker Compose, give the agent a health check and make the Caddy service
  `depends_on` it with `condition: service_healthy`.
- Under systemd, order the Caddy unit `After=` and `Requires=` the agent unit.
- Under Kubernetes, run the agent as a sidecar in the Caddy pod, and give it a
  startup probe so it is ready before Caddy's container begins.

Provisioning is deliberately loud: an unreadable or empty token file aborts
startup with an error naming the file, rather than deferring the problem to the
first handshake.

### Token file provisioning

The agent creates the token file in its own state directory and is the source of
truth for its content. Caddy only reads it.

- Give the file restrictive permissions (`0600`, or `0640` with a shared group)
  and make it readable by the user Caddy runs as.
- Mount it **read-only** into the Caddy container, for example
  `-v /var/lib/scafo/storage-token:/var/lib/scafo/storage-token:ro`. Mount the
  single file, not the agent's whole state directory.
- The token is read once during provisioning. Rotating it therefore requires a
  Caddy config reload after the agent has written the new value; reloads are
  graceful, so this is not a restart.
- The token authorizes the storage API only, and the endpoint is expected to be
  loopback or an equally private network path. Do not expose the agent's
  storage API publicly on the strength of the token alone.

### Multiple Caddy instances

Several Caddy instances may share one bucket by pointing at agents backed by the
same storage, which is how CertMagic's clustering is meant to work. Coordination
relies on the lock endpoints: exactly one instance may hold a given lock name at
a time, and the agent is responsible for expiring locks left behind by a crashed
holder. Without that expiry a crash would wedge certificate issuance for that
name until the lock is cleared by hand.

## Tracking Caddy

This module has no reason to lag behind Caddy, so it does not: a scheduled
workflow follows Caddy's stable releases on its own, and the module's own
version number stays its own.

**The pin.** `ARG CADDY_VERSION=` in the `Dockerfile` is the single canonical
statement of which Caddy this repository tracks. `go.mod`'s Caddy requirement is
held equal to it — CI fails if the two disagree — and the image, the release
binaries and the `caddy-version` release asset are all built from it.

**The automation.** Once a day, `.github/workflows/track-caddy.yml` compares the
pin against Caddy's latest stable release. If they match it exits, quietly. If
Caddy has moved it bumps `go.mod` and the pin together, runs the full check —
`go vet`, `go test -race`, and the `docker build` that compiles this module
inside xcaddy against the new Caddy — and only then pushes `chore: track caddy
vX.Y.Z` to `main`, cuts the next **patch** release of this module, and publishes
its images and binaries.

Releases therefore carry two versions that mean different things: the tag is
this module's semver, and the release notes and the `caddy-version` asset say
which Caddy is inside. A patch bump with no module changes means exactly one
thing happened — Caddy moved.

Minor and major versions are never automated. They describe changes to this
code, and stay a deliberate act.

**What a red run means.** The tracker is silent when it succeeds, so a
notification from it is always real. A failed run means the new Caddy did not
pass verification here — usually interface drift in Caddy or CertMagic. Nothing
was pushed to `main`; the bump is waiting on a `track-caddy/vX.Y.Z` branch with
a pull request open against it, and the failing run says which check broke. That
pull request's CI does not start on its own: GitHub holds runs from pull
requests opened with the default `GITHUB_TOKEN` in an approval-required state,
so use *Approve and run*, or push a commit of your own onto the branch. The
tracker keeps re-running weekly (or on manual dispatch), and keeps failing, until the bump lands or a
newer Caddy supersedes it.

**Bumping by hand.** The tracker does nothing a person cannot do directly:

```sh
go get github.com/caddyserver/caddy/v2@v2.11.4
go mod tidy
.github/scripts/caddy-pin.sh 2.11.4     # rewrites the Dockerfile ARG
go vet ./... && go test -race ./... && docker build .
```

Commit that, then publish a GitHub release when you want one. Publishing a
release by hand runs the same publishing workflow the tracker calls, so a
hand-cut release gets byte-identical assets.

## Credits

This module exists because [certmagic-s3](https://github.com/ss098/certmagic-s3)
showed how small a Caddy storage module can be — reading it is what convinced me
the agent-backed design was worth building. Thanks to the
[Caddy](https://caddyserver.com) and
[CertMagic](https://github.com/caddyserver/certmagic) maintainers and
contributors for the interfaces that make a module like this possible at all.

## Development

```sh
go vet ./...
go test ./...
docker build .                    # compiles the module inside xcaddy against a real Caddy
.github/scripts/check-pins.sh     # go.mod and the Dockerfile agree on Caddy
```

The tests run entirely against `net/http/httptest` servers and open no network
connections.
