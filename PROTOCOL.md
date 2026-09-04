# scafo node protocols v1

This is the canonical description of the wire contract between
[scafo-ingress](README.md)'s Caddy modules and a scafo node daemon. It is
normative: a daemon that satisfies everything below is a correct peer for these
modules, and these modules send nothing beyond it.

There is one protocol per module — `caddy.storage.scafo` speaks the first,
`http.reverse_proxy.upstreams.scafo` the second — and they share a daemon, a
base URL and a bearer token, nothing else. The base URL is each module's
configured `endpoint`. A path prefix on that URL is preserved, so every path
below is appended to it.

Both protocols report errors the same way; see [Errors](#errors) at the end.

## Storage protocol v1

### Common request headers

Every request carries both:

| Header | Value |
| --- | --- |
| `Authorization` | `Bearer <token>`, the trimmed token from `token` or `token_file` |
| `X-Scafo-Storage-Version` | `1` |

The version header exists so a daemon can reject a client it cannot serve.

### Key encoding

Storage keys use `/` as a component separator and have no leading or trailing
slash, for example:

```
certificates/acme-v02.api.letsencrypt.org-directory/example.com/example.com.crt
```

A key maps onto the request path as-is: each component is percent-encoded
individually and the `/` separators are preserved, so the daemon sees the same
hierarchy the caller used. Keys passed as query parameters (`list`, `stat`) use
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

### Stat body

```json
{
  "key": "certificates/acme/example.com/example.com.crt",
  "modified": "2026-04-01T12:30:00Z",
  "size": 4096,
  "isTerminal": true
}
```

`modified` is an RFC 3339 timestamp, or `null` when the daemon does not know it;
`null` becomes CertMagic's zero time, which it treats as "unknown".
`isTerminal` is `true` for keys that hold a value and `false` for keys that
merely act as a prefix for other keys ("directories").

### Lock semantics

`POST /v1/storage/locks/<name>` additionally answers `409 Conflict` when the
lock is currently held elsewhere. The client then waits one second and retries,
indefinitely, until the lock is granted or the caller's context is cancelled.
Any other error status ends the attempt immediately rather than being retried,
so a broken daemon surfaces as a failure instead of an endless poll.

Exactly one holder may hold a given lock name at a time. **The daemon is
responsible for expiring locks left behind by a crashed holder.** Without that
expiry, one crash wedges certificate issuance for that name until the lock is
cleared by hand — which is also what makes several Caddy instances able to
share one bucket, the way CertMagic's clustering is meant to work.

`DELETE /v1/storage/locks/<name>` is idempotent and answers `204` even when the
lock is not held.

### Client behaviour

The module uses a dedicated HTTP client with a 30-second per-request timeout.
That timeout does not bound `Lock`, which blocks until the lock is acquired or
its context is done.

It performs no retries other than the lock poll described above: the daemon is
colocated, so a failure is a fault to report rather than a transient to paper
over.

## Ingress protocol v1

One endpoint, read-only, polled.

| Method | Path | Request | Success |
| --- | --- | --- | --- |
| `GET` | `/v1/ingress/routes[?prefer=<self\|node-id>]` | — | `200` with the routing document |

The `Authorization: Bearer <token>` header is presented exactly as storage
presents it. There is no version header here, and deliberately so: the document
states its own version, which is a claim about the answer rather than about the
asker.

`prefer` names whose upstreams should head every route, and its value is sent
through verbatim. `self` means the node answering the request — which is what
keeps a colocated Caddy proxying to the process next to it. Any other value is
read as a node id, so `?prefer=vps-b` puts that node first wherever it appears.
An id that names no node in the fleet is not refused; the order simply stays as
it was.

**Omitting the parameter is meaningful**: it asks for the fleet's own order, and
is the only way to ask for it. The module spells that `prefer none` in its
configuration — a word it reads itself and does not send, because a literal
`none` on the wire would be read as the id of a node called "none" and quietly
do nothing.

### The routing document

```json
{
  "v": 1,
  "routes": [
    {
      "host": "app.dev.example",
      "project": "p",
      "env": "production",
      "upstreams": [
        { "node": "vps-a", "address": "10.83.0.1:41001" },
        { "node": "vps-b", "address": "10.83.0.2:41001" }
      ]
    }
  ],
  "api": {
    "hosts": ["dev.example"],
    "upstreams": [{ "node": "vps-a", "address": "127.0.0.1:7400" }],
    "denied": ["/v1/storage", "/v1/ingress/routes"]
  }
}
```

`v` is the document version and must be `1`. A document announcing any other
version is refused whole: the module keeps serving the last one it understood
rather than routing against a shape this build cannot read.

`routes` is sorted by `host` and names each host once. `project` and `env` are
carried for the operator's benefit — the module reads neither.

**`upstreams` is an ordered priority list, never a weighting.** The first entry
is where the request goes; the rest exist for when it fails. This is the whole
reason `prefer=self` is worth asking for, and the reason the module hands the
list to Caddy in document order — see
[Load balancing](README.md#load-balancing) for the `reverse_proxy` settings
that honour it.

A route with an empty `upstreams` is meaningful and is not the same as an
absent route: the host exists and has nothing running behind it right now.

`api` is absent when the node does not publish the fleet API. When present,
`hosts` are the bare platform apexes routed to the daemon itself, `upstreams`
are the daemon's own addresses ordered like a route's, and `denied` is the rule
below.

An application route always wins over an `api` host of the same name. The
daemon's renderer already omits such apexes, so this is a tie-break that should
never be needed; the module applies it anyway rather than assume it of a peer.

### Denied paths

**This is the security rule of the fleet API surface.** The paths in
`api.denied` are the daemon's own control plane. They are meant to be reachable
on loopback by whoever holds a token — this repository's modules, among
others — and they must never be reachable through the public ingress. A request
to an `api` host whose path falls under one of them is answered by the ingress
itself and is not proxied.

Matching is at a path-segment boundary: the prefix itself, or the prefix
followed by `/`. With `"/v1/storage"` denied, `/v1/storage` and
`/v1/storage/objects/x` are refused and `/v1/storagex` is not.

The check is applied to the request path both as written and with its dot
segments collapsed, and either one reaching a denied prefix is enough to refuse.
Whether the daemon's own router normalizes a path before dispatching it is not
the ingress's to assume, and erring towards refusing is the only safe direction
for this particular rule.

### Client behaviour

The module polls this endpoint every `poll_interval` (5s by default), with a
10-second per-request timeout, using a dedicated HTTP client. It waits the
interval *between* attempts rather than on a schedule, so a slow answer delays
the next request instead of stacking up behind it.

A daemon sees one poller per *configuration*, not one per `reverse_proxy`
handler. What decides the answer or its cadence — the endpoint, the token,
`prefer`, `poll_interval` — identifies a poller, and every handler asking for
the same one shares it and the document it holds. A reload hands the running
poller over rather than starting a second: the incoming configuration takes its
reference before the outgoing one gives its own back, so polling never pauses
and the last document that parsed keeps answering throughout. Polling stops when
the last handler using it is cleaned up.

The first fetch is attempted while Caddy is provisioning, so a healthy pair
starts already routing. **Its failure does not fail startup.** A daemon that is
restarting under a running ingress must not be able to keep Caddy from coming
back, so every fetch failure — at startup or later — is logged and outlived, and
the last document that parsed keeps answering requests. Failures are logged once
per state change, not once per tick.

Until a first document arrives there is nothing to route with, and the module
says so distinctly rather than reporting every host as unknown; see
[Unroutable requests](README.md#unroutable-requests).

A daemon answers `503` with `urn:scafo:draining` until its first reconcile pass
has completed: an empty document would read as every route withdrawn, where a
failed fetch is a pause that keeps the last document answering. Treat it as any
other failure, never as an empty document.

## Errors

Non-2xx responses use `application/problem+json`. The `title` and `detail`
members, when present, are surfaced in the error the module returns; otherwise
the error reports the status code alone.

For storage, a `404` is translated into an error wrapping `io/fs.ErrNotExist`,
which is what CertMagic checks with `errors.Is` to distinguish a missing key
from a broken backend. Getting this wrong makes CertMagic treat absent
certificates as infrastructure failures, so **node daemons must return `404` —
not `500` — for keys that do not exist**.
