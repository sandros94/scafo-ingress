# The integration proof

The Go tests in this repository prove the rules; this proves that the **built
image** obeys them when it is run the way an operator runs it. It needs Docker
and nothing else.

```sh
internal/e2e/run.sh            # both scenarios
internal/e2e/run.sh routing    # only the first
internal/e2e/run.sh acme       # only the second
```

It builds `scafo-caddy:multi-fleet` from the repository, builds the node
stand-in beside it, creates a network of its own, runs the scenarios, prints
one line per check and removes everything it made. A non-zero exit means a
check failed.

## What is in it

| Piece | What it is |
| --- | --- |
| `stubnode/` | a scafo node daemon stand-in: `/v1/ingress/routes` and the whole `/v1/storage` API, both behind its bearer token, plus an application backend on a second port and an unauthenticated `/_debug/keys` so a check can see WHICH node a certificate landed in |
| `Dockerfile.stub` | builds that stand-in; it imports nothing outside the standard library |
| `pebble-config.json` | Pebble with its HTTP-01 validation on port 80, so the challenge arrives at Caddy where it would in production |
| `run.sh` | the scenarios |

## The scenarios

**`routing`** runs the image with its own `/etc/caddy/Caddyfile` and **nothing
but environment variables** — two fleets, one of them `http_redirect off`. It
checks the redirect contrast, routing to each fleet's own backend, a wildcard
route, the cross-fleet claim on a host both fleets publish (and that the
conflict is logged once, not once per poll), the fleet API surface with its
denied paths, the `scafo.fleet` request variable, and the on-demand permission
refusing a name no fleet serves.

**`acme`** issues a real certificate from a local CA and checks where it
landed: in the owning fleet's storage, in no other fleet's, and with the ACME
account and CertMagic's housekeeping on the ingress's own disk.

## Two things worth knowing

**No public certificate authority is ever contacted.** Every Caddy container
runs with `SSL_CERT_FILE` pointing at a trust store holding either nothing at
all or Pebble's root alone, so a request to any other CA fails at the
handshake rather than arriving. The `routing` scenario additionally never
follows a redirect to HTTPS, so no handshake for a name the permission module
*allows* happens there at all. Both scenarios assert that the logs name no
public CA.

**The `acme` scenario mounts a Caddyfile.** It is the image's own with one
line added — `acme_ca` — because Caddy's *default* certificate authority is
not settable from the environment, and this must not be a public one. Every
other setting still comes from the environment, and the fleets are configured
exactly as in the first scenario.

## What it does not cover

The wildcard certificate automation (`wildcards` in the routing document,
provider selection by zone, the ACME delegation) is proved by the table tests
in `fleets/`, not here: solving a DNS-01 challenge against Pebble's
`challtestsrv` needs a Caddy DNS provider module that speaks its management
API, and none of the curated providers does. See the wildcard section of
[README.md](../../README.md) for what is and is not automatic.
