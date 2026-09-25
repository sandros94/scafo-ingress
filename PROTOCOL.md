# scafo node protocols v1

This is the canonical description of the wire contract between
[scafo-ingress](README.md)'s Caddy modules and a scafo node daemon. It is
normative: a daemon that satisfies everything below is a correct peer for these
modules, and these modules send nothing beyond it.

There are two protocols — `caddy.storage.scafo` speaks the first, the routing
poller behind `http.reverse_proxy.upstreams.scafo` and `tls.permission.scafo`
speaks the second — and they share a daemon, a base URL and a bearer token,
nothing else. The base URL and the token are one **fleet**'s, as the `scafo` app
declares it. A path prefix on that URL is preserved, so every path below is
appended to it.

One ingress may hold several fleets at once, each with its own endpoint, token
and document. **That is the consumer's concern, not the daemon's**: a daemon
answers for itself and knows nothing about any other, and everything about how
several documents are combined is in
[Several fleets at once](#several-fleets-at-once) at the end.

Both protocols report errors the same way; see [Errors](#errors).

## Storage protocol v1

### Common request headers

Every request carries both:

| Header | Value |
| --- | --- |
| `Authorization` | `Bearer <token>`, the trimmed token from that fleet's `token` or `token_file` |
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

**Not every key CertMagic produces reaches a daemon.** Keys that name a domain
are routed to the fleet that owns that domain; keys that name none — the ACME
account, CertMagic's housekeeping, Caddy's own PKI and ECH assets — stay on the
ingress's local disk and are never sent anywhere. That split is the ingress's,
not the protocol's; see
[Storage](README.md#storage-a-certificate-never-crosses-a-fleet).

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
found", not as an empty result, and the ingress relies on it when it merges the
listings of several fleets.

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

The only lock name a daemon sees is `issue_cert_<domain>`, for a domain it owns.
CertMagic's other locks name no domain and are taken on the ingress's local
disk.

### Client behaviour

Each fleet gets a dedicated HTTP client whose per-request timeout is that
fleet's `storage_timeout` (30 seconds by default). That timeout does not bound
`Lock`, which blocks until the lock is acquired or its context is done.

It performs no retries other than the lock poll described above: a failure is a
fault to report rather than a transient to paper over.

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
      "application": "p",
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
  },
  "wildcards": ["dev.example"]
}
```

`v` is the document version and must be `1`. A document announcing any other
version is refused whole: the module keeps serving the last one it understood
rather than routing against a shape this build cannot read.

`routes` is sorted by `host` and names each host once. `application` is
carried for the operator's benefit — the module does not read it.

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
below. They are exact names; a wildcard is a route's to carry and never an
`api` host's.

### Wildcard hosts

A route's `host` may be a wildcard instead of a name: **exactly one leading
`*.` label**, followed by a base that holds no `*` of its own, as in
`*.example.com`. It is what an operator gets by claiming a custom domain
together with its subdomains, and it is the only pattern the document may use —
there is no other metacharacter and no other position for this one.

**A wildcard covers exactly one label.** `*.example.com` covers
`a.example.com`, and neither `b.a.example.com` nor `example.com` itself.

A `host` carrying `*` in any other shape — `*` or `*.` alone, `a.*.example.com`,
`*.*.example.com`, `*x.example.com` — is not a wildcard and is not a name
either. The entry is ignored, as every other entry the module cannot use is,
and it is indexed under neither reading: a pattern the two sides would resolve
differently is the one thing worth refusing outright.

### `wildcards`

`wildcards` is an **optional** top-level member: an array of bases the fleet
wants a `*.<base>` certificate for — its platform domains, and every custom
domain claimed together with its subdomains.

It is a list of **names**, not patterns: the base `example.com` asks for the
certificate `*.example.com`. An entry carrying a `*` is a shape this build does
not read and is dropped, as an unreadable route host is. Entries are lowercased,
trimmed and deduplicated; a repeated base is one base.

When a base also appears in that document's `api.hosts`, the bare name is wanted
as well as the wildcard, because `*.example.com` does not cover `example.com`
and the platform answers on both.

**A node that publishes no `wildcards` keeps working exactly as before.** The
member is absent, nothing is managed for that fleet, and its names are covered
by on-demand issuance for the hosts it routes exactly. What the consumer does
with the list, and what it cannot do, is in
[Wildcard certificates](README.md#wildcard-certificates).

`wildcards` is the fleet's *request*, not a claim about what exists: a base in
it is not a route, does not make a host servable, and takes part in no routing
decision. Its only other effect is on ownership — a `*.base` certificate belongs
to the fleet that asked for `base`, when no fleet carries `*.base` as a route
and none answers on `base` as a platform apex.

### Resolving a host

A request's host, lowercased and with any port removed, is resolved against the
document in this order. The first match answers, and nothing else is consulted:

1. **the route naming it exactly.** A route with an empty `upstreams` matches
   here like any other: "this host exists and has nothing running" is an
   answer, not an absence, and it never falls through to a wildcard.
2. **an `api` host naming it exactly**, subject to [Denied paths](#denied-paths)
   below. An application route therefore wins over an `api` host of the same
   name — the daemon's renderer already omits such apexes, so that tie-break
   should never be needed and the module applies it rather than assume it of a
   peer — while a wildcard never takes a name away from the platform API.
3. **the wildcard covering it**: the host with its first label replaced by `*`,
   looked up once. Once, and not walked up the name, which is the same rule as
   "a wildcard covers exactly one label" seen from the request's side.

A host that none of the three matches is not served by the fleet.

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

Each fleet is polled every `poll_interval` (5s by default), with a per-fetch
timeout of `timeout` (10s by default), using a dedicated HTTP client. The
interval is waited *between* attempts rather than on a schedule, so a slow
answer delays the next request instead of stacking up behind it.

A daemon sees **one poller per fleet**, not one per `reverse_proxy` handler —
however many site blocks use `dynamic scafo`, the fetch happens once per
interval. A reload hands the running poller over rather than starting a second:
the incoming configuration takes its reference before the outgoing one gives its
own back, so polling never pauses and the last document that parsed keeps
answering throughout. Polling stops when the last configuration using that fleet
has gone.

A fetch whose bytes are identical to the last one's is **dropped rather than
republished**. Nothing downstream can tell the difference, and it is what keeps
work that depends on a change — rebuilding the cross-fleet view, logging a host
conflict, reconsidering the wildcards — from happening once per poll.

The first fetch is attempted while Caddy is provisioning, so a healthy pair
starts already routing. **Its failure does not fail startup.** A daemon that is
restarting under a running ingress must not be able to keep Caddy from coming
back, so every fetch failure — at startup or later — is logged and outlived, and
the last document that parsed keeps answering requests. Failures are logged once
per state change, not once per tick.

A daemon answers `503` with `urn:scafo:draining` until its first reconcile pass
has completed: an empty document would read as every route withdrawn, where a
failed fetch is a pause that keeps the last document answering. Treat it as any
other failure, never as an empty document.

## Several fleets at once

Everything in this section is the **consumer's**, not the daemon's. A daemon
serves one fleet and is entitled to assume nothing about any other; these are
the rules the ingress applies when it has more than one document in hand.

**Resolution spans every fleet at once**, in the order
[Resolving a host](#resolving-a-host) gives: an exact route in any fleet, then a
fleet API host in any fleet, then a wildcard route in any fleet. So an **exact
route beats a wildcard across fleets**, exactly as it does within one.

**A claim is sticky.** When two fleets publish the same host, the one that
published it *first* keeps it for as long as it goes on publishing it. Only when
that fleet stops does another get it, and when the first comes back the second
keeps it — a host never flaps between two fleets. Two fleets claiming a host in
the same refresh have no "first", so the order the fleets were configured in
decides.

Routes, `api` hosts and wildcards are **separate keyspaces**. One fleet holding
`*.example` and another holding `a.example` is not a conflict; neither is one
fleet routing `example.com` while another answers on it as a platform apex,
though the route wins for a request.

**A conflict is logged once per change**, naming the host, which keyspace it is
in, which fleet serves it and which fleets claim it — and once more when it is
resolved. Since a document that did not change is not republished, a standing
conflict leaves one line behind rather than one per poll.

**A fleet whose node is unreachable affects only its own hosts.** Its last
document keeps answering; the other fleets neither know nor care.

**`unavailable` means not one fleet has a document yet.** As soon as any fleet
has answered, a host nobody names reads as `unknown` — including a host that
belongs to a fleet that has not answered yet, which the ingress has no way to
know about. A cold start is a distinct state; a partially cold start is not.

**Ownership decides where a certificate lives**, and it is this same resolution
plus one rule for a `*.base` subject: the fleet carrying `*.base` as a route,
else the fleet answering on `base` as a platform apex, else a fleet that listed
`base` in its `wildcards`. A domain nobody owns has no fleet, and the ingress
refuses to guess one.

## Errors

Non-2xx responses use `application/problem+json`. The `title` and `detail`
members, when present, are surfaced in the error the module returns; otherwise
the error reports the status code alone.

For storage, a `404` is translated into an error wrapping `io/fs.ErrNotExist`,
which is what CertMagic checks with `errors.Is` to distinguish a missing key
from a broken backend. Getting this wrong makes CertMagic treat absent
certificates as infrastructure failures, so **node daemons must return `404` —
not `500` — for keys that do not exist**.
