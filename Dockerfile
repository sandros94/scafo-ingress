# syntax=docker/dockerfile:1

# Builds a Caddy image with the scafo storage module compiled in.
#
# CADDY_VERSION selects both the xcaddy builder and the runtime base, so the
# two never drift apart. It defaults to the 2.x floating tag; pin it to an
# exact release (e.g. --build-arg CADDY_VERSION=2.11.4) for reproducible
# builds.
ARG CADDY_VERSION=2

FROM caddy:${CADDY_VERSION}-builder AS builder

# The module is compiled from this build context rather than fetched from the
# module proxy, so the image always matches the tree it was built from.
COPY . /src

# The builder image pins the Caddy version through its own CADDY_VERSION
# environment variable, so xcaddy needs no version argument here.
RUN xcaddy build --with github.com/sandros94/scafo-certmagic=/src

FROM caddy:${CADDY_VERSION}

COPY --from=builder /usr/bin/caddy /usr/bin/caddy
