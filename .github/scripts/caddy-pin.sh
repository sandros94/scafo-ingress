#!/usr/bin/env bash
#
# Reads or rewrites the canonical Caddy pin.
#
# The pin lives in exactly one place: the `ARG CADDY_VERSION=` default in the
# Dockerfile. The image, the release binaries, the `caddy-version` release
# asset and go.mod's Caddy requirement are all derived from it, so "which Caddy
# does this repository track" has a single answer that a machine can read.
#
#   caddy-pin.sh            prints the pinned version, e.g. 2.11.4
#   caddy-pin.sh 2.12.0     rewrites the ARG default, then prints it
#
# A leading "v" is accepted on input (Caddy's git tags carry one) and stripped:
# the pin is a Docker tag, and Docker tags do not.

set -euo pipefail

dockerfile="${DOCKERFILE:-Dockerfile}"

read_pin() {
	local line
	line="$(grep -m1 -E '^ARG CADDY_VERSION=' "$dockerfile" || true)"
	if [ -z "$line" ]; then
		echo "caddy-pin: no 'ARG CADDY_VERSION=' line in $dockerfile" >&2
		exit 1
	fi
	printf '%s\n' "${line#ARG CADDY_VERSION=}"
}

if [ "$#" -eq 0 ]; then
	read_pin
	exit 0
fi

version="${1#v}"
if ! [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
	echo "caddy-pin: '$1' is not an exact MAJOR.MINOR.PATCH version" >&2
	exit 1
fi

# Rewritten in place rather than appended to, so the surrounding comment block
# stays attached to the line it explains.
sed -i -E "s|^ARG CADDY_VERSION=.*|ARG CADDY_VERSION=${version}|" "$dockerfile"
read_pin
