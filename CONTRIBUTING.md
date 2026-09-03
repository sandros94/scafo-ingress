# Contributing

Everything maintenance-facing for [scafo-ingress](README.md): how to run the
checks, and how this repository follows Caddy's releases.

## Layout

One Go module, one Caddy binary, a package per Caddy module:

| Path | What lives there |
| --- | --- |
| `scafo.go` | the repository root, which imports every module below so that naming the root in `xcaddy build` compiles them all |
| `storage/` | `caddy.storage.scafo`, the certmagic storage client |
| `ingress/` | `http.reverse_proxy.upstreams.scafo`, the routing-document poller and the dynamic upstreams source over it |
| `internal/nodeapi/` | what every module needs to reach the node daemon: the `endpoint`/`token`/`token_file` options and their resolution, and the daemon's problem+json errors |
| `examples/` | a Caddyfile using both modules together, kept working by hand rather than by CI |
| `dns-providers.txt` | the DNS-01 providers compiled into the `-dns` flavour, one pinned Go module per line |

`internal/nodeapi` holds what is shared *because* the daemon is shared, and
nothing else. A module's own paths, headers and protocol version belong to that
module, even when a second one ends up wanting something similar.

Each module package is split the same way: the client and its types in a file
named after the package, the Caddy module — registration, options, provisioning,
Caddyfile — in `caddy.go`.

`go.mod` is long out of proportion to this repository, and `ingress/` is why:
`modules/caddyhttp/reverseproxy` is not importable without `caddyhttp`, which
pulls in Caddy's own dependency tree. Nothing here uses any of it directly, and
the linker drops what a given build does not reach.

## Development

```sh
go vet ./...
go test ./...
docker build .                    # compiles the modules inside xcaddy against a real Caddy
.github/scripts/check-pins.sh     # go.mod and the Dockerfile agree on Caddy

# the -dns flavour, which is the same build with one argument added
docker build --build-arg DNS_PROVIDERS="$(.github/scripts/dns-providers.sh)" .
```

The tests run entirely against `net/http/httptest` servers and open no network
connections. `docker build .` is the integration check: it is the only step that
proves the modules still compile into a real Caddy binary, so run it before
touching anything that faces Caddy's or CertMagic's interfaces.

The workflows are linted too, and their inline shell along with them:

```sh
docker run --rm -v "$PWD":/repo -w /repo rhysd/actionlint:latest
shellcheck .github/scripts/*.sh
```

### Running the Go toolchain in Docker

This repository's `go` directive tracks Caddy's, which is routinely newer than
the Go a distribution ships. When the local toolchain is too old, run it in a
container instead of installing a second Go by hand:

```sh
docker run --rm -v "$PWD":/w -w /w golang:1.25 go vet ./...
docker run --rm -v "$PWD":/w -w /w golang:1.25 go test -race ./...
```

`docker build .` and `check-pins.sh` need no such wrapper — the first brings its
own toolchain in the builder image, the second is shell and `sed`.

## The two flavours

Everything published exists twice: **lean**, this repository's modules and
nothing else, and **`-dns`**, the same build with the DNS-01 providers compiled
in. They are one Dockerfile and one xcaddy invocation apart — the `DNS_PROVIDERS`
build argument, whitespace-separated pinned module paths, empty by default.

**The list.** `dns-providers.txt` is the single statement of which providers
those are, one pinned module per line, comments above each entry.
`.github/scripts/dns-providers.sh` is its only parser: bare, it prints the list
for `DNS_PROVIDERS`; with `--modules`, the `dns.providers.<name>` each entry is
expected to register, which the release build asserts against
`caddy list-modules`. A provider that compiles but does not register — a renamed
module, a broken `init` — fails there rather than shipping quietly.

**Bumping a pin.** Deliberately manual, which is the point of pinning:

```sh
gh api repos/caddy-dns/ovh/releases/latest --jq .tag_name   # or /tags for repos without releases
$EDITOR dns-providers.txt
docker build --build-arg DNS_PROVIDERS="$(.github/scripts/dns-providers.sh)" -t scafo-caddy-dns .
docker run --rm scafo-caddy-dns caddy list-modules | grep dns.providers
```

Two shapes of pin appear in that file, and both are exact. Most providers are
tagged, so the pin is a semver — note that a v2+ line is a distinct Go module
and carries the `/v2` path suffix. A provider that has never tagged a release
takes the Go pseudo-version of a commit, which names one revision just as
precisely; `curl -s https://proxy.golang.org/<module>/@latest` prints the
current one.

**Where the flavours enter CI.** `flavor` is a matrix dimension in `ci.yml` and
`publish.yml`, never a second pass: the two builds run beside each other, and on
the publishing path so do the four (flavour × architecture) image builds and the
four binary builds. Both dimensions are spelled out as real matrix keys —
a GitHub `include:` entry whose keys are all new is merged into *every*
combination instead of making one of its own, which would silently halve the
matrix.

`image-tags.sh --flavor dns` adds the `-dns` suffix, so a tag's shape stays
decided in one place; callers compute both lists and hand both to `publish.yml`.
The lean names are unchanged and must stay so: scafo's `install.sh` downloads
`scafo-caddy-<os>-<arch>` by that exact name, and the providers arrive under
`scafo-caddy-dns-<os>-<arch>` beside it.

## Tracking Caddy

These modules have no reason to lag behind Caddy, so they do not: a scheduled
workflow follows Caddy's stable releases on its own, and this repository's own
version number stays its own.

**The pin.** `ARG CADDY_VERSION=` in the `Dockerfile` is the single canonical
statement of which Caddy this repository tracks. `go.mod`'s Caddy requirement is
held equal to it — CI fails if the two disagree — and the image, the release
binaries and the `caddy-version` release asset are all built from it.

**The automation.** Once a day, `.github/workflows/track-caddy.yml` compares the
pin against Caddy's latest stable release. If they match it exits, quietly. If
Caddy has moved it bumps `go.mod` and the pin together, runs the full check —
`go vet`, `go test -race`, and the `docker build`s that compile these modules
inside xcaddy against the new Caddy, both flavours, since a new Caddy can break
a pinned provider as readily as it can break these modules — and only then
pushes `chore: track caddy vX.Y.Z` to `main`, cuts the next **patch** release of
this repository, and publishes its images and binaries.

Releases therefore carry two versions that mean different things: the tag is
this repository's semver, and the release notes and the `caddy-version` asset
say which Caddy is inside. A patch bump with no module changes means exactly one
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
tracker keeps re-running weekly (or on manual dispatch), and keeps failing,
until the bump lands or a newer Caddy supersedes it.

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
