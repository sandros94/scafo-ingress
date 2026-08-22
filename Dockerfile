# syntax=docker/dockerfile:1

# Builds a Caddy image with the scafo storage module compiled in.
#
# CADDY_VERSION selects both the xcaddy builder and the runtime base, so the
# two never drift apart. Override it (--build-arg CADDY_VERSION=2.10.0) to
# build against a different release.
#
# This default is the canonical Caddy pin for the whole repository: go.mod's
# Caddy requirement is kept equal to it, the release binaries are built from
# it, and .github/workflows/track-caddy.yml reads and rewrites this exact line.
# It is an exact version rather than the floating "2" tag on purpose -- a
# floating tag would silently pick up new Caddy releases, which is precisely
# what the tracker exists to do deliberately, test, and record in a release.
ARG CADDY_VERSION=2.11.4

FROM caddy:${CADDY_VERSION}-builder AS builder

# The module is compiled from this build context rather than fetched from the
# module proxy, so the image always matches the tree it was built from.
COPY . /src

# The builder image pins the Caddy version through its own CADDY_VERSION
# environment variable, so xcaddy needs no version argument here.
RUN xcaddy build --with github.com/sandros94/scafo-certmagic=/src

FROM caddy:${CADDY_VERSION}

COPY --from=builder /usr/bin/caddy /usr/bin/caddy
