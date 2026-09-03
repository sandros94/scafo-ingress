#!/usr/bin/env bash
#
# Prints the image references a publishing run should tag, one per line.
#
#   image-tags.sh --image ghcr.io/o/n --sha "$GITHUB_SHA"
#   image-tags.sh --image ghcr.io/o/n --version v1.2.3 --latest
#   image-tags.sh --image ghcr.io/o/n --sha "$GITHUB_SHA" --flavor dns
#
# Every publishing path routes through here so the tag *shape* is defined once.
# Which tags a path asks for is the caller's decision:
#
#   push to main       --sha                      -> :edge, :sha-1234567
#   release published  --version [--latest]       -> :1.2.3 [, :latest]
#   caddy tracker      --version --latest --sha   -> all four, because the
#                                                    commit it releases is also
#                                                    the new tip of main
#
# --latest is a separate flag rather than implied by --version: a prerelease
# must never move the :latest pointer.
#
# --flavor names which build the tags are for, and suffixes every one of them.
# "lean" (the default) is Caddy with only this repository's modules and takes no
# suffix, so its tags are byte-identical to what this script printed before
# flavours existed. "dns" adds the providers in dns-providers.txt and yields
# :edge-dns, :1.2.3-dns, :latest-dns. The suffix belongs here rather than to
# whoever passes the list on, so that the tag shape stays defined in one place.

set -euo pipefail

image=""
version=""
sha=""
latest=false
flavor="lean"

while [ "$#" -gt 0 ]; do
	case "$1" in
	--image)
		image="${2-}"
		shift 2
		;;
	--version)
		version="${2-}"
		shift 2
		;;
	--sha)
		sha="${2-}"
		shift 2
		;;
	--latest)
		latest=true
		shift
		;;
	--flavor)
		flavor="${2-}"
		shift 2
		;;
	*)
		echo "image-tags: unknown argument '$1'" >&2
		exit 1
		;;
	esac
done

if [ -z "$image" ]; then
	echo "image-tags: --image is required" >&2
	exit 1
fi

if [ -z "$version" ] && [ -z "$sha" ]; then
	echo "image-tags: at least one of --version or --sha is required" >&2
	exit 1
fi

# The flavours are enumerated rather than turned straight into a suffix, so a
# typo produces an error here instead of a plausible-looking tag nobody asked
# for -- ":latest-dsn" would publish perfectly happily.
case "$flavor" in
lean) suffix="" ;;
dns) suffix="-dns" ;;
*)
	echo "image-tags: unknown flavor '$flavor'; expected lean or dns" >&2
	exit 1
	;;
esac

tags=()

if [ -n "$version" ]; then
	# Image tags carry the bare semver, without the git tag's "v".
	tags+=("${image}:${version#v}${suffix}")
	if [ "$latest" = true ]; then
		tags+=("${image}:latest${suffix}")
	fi
fi

if [ -n "$sha" ]; then
	tags+=("${image}:edge${suffix}" "${image}:sha-${sha:0:7}${suffix}")
fi

printf '%s\n' "${tags[@]}"
