#!/usr/bin/env bash
#
# Prints caddyserver/caddy's latest STABLE release tag, e.g. v2.11.4.
#
# `/releases/latest` is the endpoint that already answers "latest stable": the
# REST API defines it as "the most recent non-prerelease, non-draft release",
# so no client-side filtering is needed and a Caddy beta can never become the
# tracked version.
#
# Caveat worth knowing: that endpoint sorts by `created_at`, which is the date
# of the *commit* behind the release rather than the publish date. A patch cut
# from an older commit than a newer minor would therefore not be reported as
# latest. Caddy has never released that way, and the alternative (sorting all
# releases ourselves) would have to re-implement the prerelease filtering that
# this endpoint gets right.

set -euo pipefail

repo="${CADDY_REPO:-caddyserver/caddy}"

tag="$(gh api "repos/${repo}/releases/latest" --jq '.tag_name')"

# The pin is substituted into a Docker tag and a `go get` argument, so anything
# other than an exact vMAJOR.MINOR.PATCH is refused rather than propagated.
if ! [[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
	echo "caddy-latest: unexpected release tag '$tag' from ${repo}" >&2
	exit 1
fi

printf '%s\n' "$tag"
