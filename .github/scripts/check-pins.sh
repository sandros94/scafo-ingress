#!/usr/bin/env bash
#
# Fails when the two Caddy versions in this repository disagree.
#
# The Dockerfile's ARG selects the xcaddy builder and the runtime base; go.mod's
# requirement selects what this module compiles against. xcaddy resolves the
# higher of the two, so a mismatch does not fail the build -- it silently ships
# an image whose Caddy is not the one the tests ran against. The tracker always
# moves both together; this is what catches a hand edit that moved only one.

set -euo pipefail

pin="$("$(dirname "$0")/caddy-pin.sh")"

gomod="$(sed -n -E 's|^[[:space:]]*github\.com/caddyserver/caddy/v2 (v[0-9]+\.[0-9]+\.[0-9]+).*$|\1|p' go.mod | head -n 1)"

if [ -z "$gomod" ]; then
	echo "check-pins: no exact github.com/caddyserver/caddy/v2 requirement in go.mod" >&2
	exit 1
fi

if [ "v${pin}" != "$gomod" ]; then
	echo "check-pins: Dockerfile pins Caddy v${pin} but go.mod requires ${gomod}" >&2
	exit 1
fi

echo "check-pins: Caddy v${pin} in both Dockerfile and go.mod"
