#!/usr/bin/env bash
#
# Reads the curated DNS provider list, one entry per line.
#
#   dns-providers.sh              github.com/caddy-dns/bunny@v1.2.0 ...
#   dns-providers.sh --modules    dns.providers.bunny ...
#
# The list itself lives in dns-providers.txt, which is the reviewable half; this
# is only the parser, so that "which providers" has one answer and the comment
# syntax is honoured in one place. The default form is what the Dockerfile's
# DNS_PROVIDERS build argument takes:
#
#   docker build --build-arg DNS_PROVIDERS="$(.github/scripts/dns-providers.sh)" .
#
# --modules prints the Caddy module each entry is expected to register, derived
# from the caddy-dns naming convention (last path element, minus any /vN). It is
# what the publishing workflow asserts against `caddy list-modules`, so that a
# provider which silently fails to register fails the build instead.

set -euo pipefail

modules=false

while [ "$#" -gt 0 ]; do
	case "$1" in
	--modules)
		modules=true
		shift
		;;
	*)
		echo "dns-providers: unknown argument '$1'" >&2
		exit 1
		;;
	esac
done

list="${DNS_PROVIDERS_FILE:-$(dirname "$0")/../../dns-providers.txt}"

if [ ! -f "$list" ]; then
	echo "dns-providers: no provider list at $list" >&2
	exit 1
fi

entries=()

while IFS= read -r line; do
	# Comments are whole lines, so an entry is never truncated by a stray "#".
	line="${line%%$'\r'}"
	case "$line" in
	'' | '#'*) continue ;;
	esac
	# Whitespace on an entry line would become a second --with argument, which
	# would fail far away from the cause; reject it here instead.
	if [ "$line" != "${line// /}" ] || [ "$line" != "${line//$'\t'/}" ]; then
		echo "dns-providers: '$line' contains whitespace; one module per line" >&2
		exit 1
	fi
	# The pin is the point of the file: an unpinned entry would resolve to
	# whatever the proxy served that day.
	case "$line" in
	*@*) ;;
	*)
		echo "dns-providers: '$line' has no @version; pins are exact" >&2
		exit 1
		;;
	esac
	entries+=("$line")
done <"$list"

if [ "${#entries[@]}" -eq 0 ]; then
	echo "dns-providers: $list lists no providers" >&2
	exit 1
fi

if [ "$modules" = false ]; then
	printf '%s\n' "${entries[@]}"
	exit 0
fi

for entry in "${entries[@]}"; do
	path="${entry%@*}"
	# A major-version suffix is part of the module path, never of the name.
	path="$(printf '%s\n' "$path" | sed -E 's|/v[0-9]+$||')"
	printf 'dns.providers.%s\n' "${path##*/}"
done
