# scafo storage protocol v1

This is the canonical description of the wire contract between
[scafo-certmagic](README.md) and a scafo agent. It is normative: an agent that
satisfies everything below is a correct peer for this module, and this module
sends nothing beyond it.

The base URL is the module's configured `endpoint`. A path prefix on that URL is
preserved, so every path below is appended to it.

## Common request headers

Every request carries both:

| Header | Value |
| --- | --- |
| `Authorization` | `Bearer <token>`, the trimmed token from `token` or `token_file` |
| `X-Scafo-Storage-Version` | `1` |

The version header exists so an agent can reject a client it cannot serve.

## Key encoding

Storage keys use `/` as a component separator and have no leading or trailing
slash, for example:

```
certificates/acme-v02.api.letsencrypt.org-directory/example.com/example.com.crt
```

A key maps onto the request path as-is: each component is percent-encoded
individually and the `/` separators are preserved, so the agent sees the same
hierarchy the caller used. Keys passed as query parameters (`list`, `stat`) use
ordinary form encoding.

## Endpoints

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

## Stat body

```json
{
  "key": "certificates/acme/example.com/example.com.crt",
  "modified": "2026-04-01T12:30:00Z",
  "size": 4096,
  "isTerminal": true
}
```

`modified` is an RFC 3339 timestamp, or `null` when the agent does not know it;
`null` becomes CertMagic's zero time, which it treats as "unknown".
`isTerminal` is `true` for keys that hold a value and `false` for keys that
merely act as a prefix for other keys ("directories").

## Lock semantics

`POST /v1/storage/locks/<name>` additionally answers `409 Conflict` when the
lock is currently held elsewhere. The client then waits one second and retries,
indefinitely, until the lock is granted or the caller's context is cancelled.
Any other error status ends the attempt immediately rather than being retried,
so a broken agent surfaces as a failure instead of an endless poll.

Exactly one holder may hold a given lock name at a time. **The agent is
responsible for expiring locks left behind by a crashed holder.** Without that
expiry, one crash wedges certificate issuance for that name until the lock is
cleared by hand — which is also what makes several Caddy instances able to
share one bucket, the way CertMagic's clustering is meant to work.

`DELETE /v1/storage/locks/<name>` is idempotent and answers `204` even when the
lock is not held.

## Errors

Non-2xx responses use `application/problem+json`. The `title` and `detail`
members, when present, are surfaced in the error the module returns; otherwise
the error reports the status code alone.

A `404` is translated into an error wrapping `io/fs.ErrNotExist`, which is what
CertMagic checks with `errors.Is` to distinguish a missing key from a broken
backend. Getting this wrong makes CertMagic treat absent certificates as
infrastructure failures, so **agents must return `404` — not `500` — for keys
that do not exist**.

## Client behaviour

The module uses a dedicated HTTP client with a 30-second per-request timeout.
That timeout does not bound `Lock`, which blocks until the lock is acquired or
its context is done.

It performs no retries other than the lock poll described above: the agent is
colocated, so a failure is a fault to report rather than a transient to paper
over.
