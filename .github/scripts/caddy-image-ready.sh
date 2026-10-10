#!/usr/bin/env bash
#
# Answers whether docker.io/library/caddy has published the two tags a bump to
# the given Caddy version builds from: `<version>-builder` for the xcaddy stage
# and `<version>` for the runtime base of the Dockerfile.
#
# The GitHub release and the official image are published by different hands,
# and the image can lag the tag by days. A tracker that moved the pin inside
# that gap would pass every Go check and then fail the docker build on a tag
# that does not exist yet -- a red run about nothing this repository can fix.
# So the tracker asks this first, and treats "not yet" as nothing to do.
#
# Asked of the registry the build pulls from, not of hub.docker.com's catalog:
# a HEAD of the manifest is exactly what `docker build` resolves, and HEAD
# requests do not count against Docker Hub's pull rate limit.
#
# Exit 0 when both tags are there, 1 when at least one is missing (each one
# named on stderr), 2 when the registry could not be asked at all.

set -euo pipefail

if [ $# -ne 1 ] || ! [[ "$1" =~ ^v?[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
	echo "usage: caddy-image-ready.sh vX.Y.Z" >&2
	exit 2
fi
version="${1#v}"
repository="library/caddy"

registry="${CADDY_IMAGE_REGISTRY:-https://registry-1.docker.io}"
auth="${CADDY_IMAGE_AUTH:-https://auth.docker.io/token?service=registry.docker.io&scope=repository:${repository}:pull}"

token="$(curl -fsS --max-time 30 "$auth" | jq -r '.token // empty')" || token=""
if [ -z "$token" ]; then
	echo "caddy-image-ready: no pull token from ${auth}" >&2
	exit 2
fi

# Every manifest type the official image has ever been published as; without
# them the registry answers 404 for a tag it does hold.
accept='application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json'

missing=0
for tag in "${version}-builder" "${version}"; do
	status="$(curl -sS --max-time 30 -o /dev/null -w '%{http_code}' -I \
		-H "Authorization: Bearer ${token}" -H "Accept: ${accept}" \
		"${registry}/v2/${repository}/manifests/${tag}")" || status="000"
	case "$status" in
	200) ;;
	404)
		echo "caddy-image-ready: docker.io/${repository}:${tag} is not published yet" >&2
		missing=1
		;;
	*)
		echo "caddy-image-ready: HEAD of docker.io/${repository}:${tag} answered ${status}" >&2
		exit 2
		;;
	esac
done

if [ "$missing" -ne 0 ]; then
	exit 1
fi
echo "caddy-image-ready: docker.io/${repository}:${version} and :${version}-builder are published"
