#!/usr/bin/env bash
#
# Reads git tag names on stdin and prints the next PATCH release tag.
#
#   git tag --list | next-version.sh     ->  v0.1.3
#
# The module's version is the module's own: the tracker may only ever add one
# to the patch component. Minor and major bumps describe changes to this code
# and stay a human decision, as does the very first release -- when stdin holds
# no release tag this prints nothing and exits 0, which the tracker reads as
# "publish images, cut no release".
#
# Only exact vMAJOR.MINOR.PATCH tags are considered. A prerelease or
# build-metadata tag is deliberately invisible here so the tracker cannot bump
# onto one.

set -euo pipefail

latest="$(grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -n 1 || true)"

[ -n "$latest" ] || exit 0

IFS=. read -r major minor patch <<<"${latest#v}"

printf 'v%s.%s.%s\n' "$major" "$minor" "$((patch + 1))"
