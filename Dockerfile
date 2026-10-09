# syntax=docker/dockerfile:1

# Builds a Caddy image with the scafo modules compiled in.
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
ARG CADDY_VERSION=2.11.7

FROM caddy:${CADDY_VERSION}-builder AS builder

# Extra Caddy modules to compile in, as whitespace-separated Go module paths,
# each pinned (`github.com/caddy-dns/ovh@v1.1.0`). Empty by default, which is
# the lean flavour published as :edge, :X.Y.Z and :latest.
#
# The curated DNS-01 set behind the :…-dns flavour is not written here: it lives
# in dns-providers.txt, and the workflows pass it in. Anyone can pass a
# different one:
#
#   docker build --build-arg DNS_PROVIDERS="$(.github/scripts/dns-providers.sh)" .
#   docker build --build-arg DNS_PROVIDERS=github.com/caddy-dns/gandi@v1.0.3 .
ARG DNS_PROVIDERS=""

# The modules are compiled from this build context rather than fetched from the
# module proxy, so the image always matches the tree it was built from.
COPY . /src

# The repository root imports every module in it, so naming the root here is
# what compiles them all in. The builder image pins the Caddy version through
# its own CADDY_VERSION environment variable, so xcaddy needs no version
# argument.
#
# The providers are appended as positional arguments rather than interpolated
# into one command line, so that the only place a value is split on whitespace
# is the loop that means to split it.
RUN set -eu; \
	set -- --with github.com/sandros94/scafo-ingress=/src; \
	for provider in ${DNS_PROVIDERS}; do \
		set -- "$@" --with "${provider}"; \
	done; \
	xcaddy build "$@"

FROM caddy:${CADDY_VERSION}

COPY --from=builder /usr/bin/caddy /usr/bin/caddy

# The default configuration, which needs nothing but environment variables:
# no Caddyfile to upload, no volume to bind. Mounting a Caddyfile of your own
# over /etc/caddy/Caddyfile replaces it.
COPY image/Caddyfile /etc/caddy/Caddyfile
