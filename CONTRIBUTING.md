# Contributing

Everything maintenance-facing for [scafo-certmagic](README.md): how to run the
checks, and how this repository follows Caddy's releases.

## Development

```sh
go vet ./...
go test ./...
docker build .                    # compiles the module inside xcaddy against a real Caddy
.github/scripts/check-pins.sh     # go.mod and the Dockerfile agree on Caddy
```

The tests run entirely against `net/http/httptest` servers and open no network
connections. `docker build .` is the integration check: it is the only step that
proves the module still compiles into a real Caddy binary, so run it before
touching anything that faces Caddy's or CertMagic's interfaces.

### Running the Go toolchain in Docker

This module's `go` directive tracks Caddy's, which is routinely newer than the
Go a distribution ships. When the local toolchain is too old, run it in a
container instead of installing a second Go by hand:

```sh
docker run --rm -v "$PWD":/w -w /w golang:1.25 go vet ./...
docker run --rm -v "$PWD":/w -w /w golang:1.25 go test -race ./...
```

`docker build .` and `check-pins.sh` need no such wrapper — the first brings its
own toolchain in the builder image, the second is shell and `sed`.

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
