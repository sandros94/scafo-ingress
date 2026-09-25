#!/usr/bin/env bash
# The integration proof: the BUILT image, two scafo node stand-ins, and a
# local ACME CA.
#
# Two scenarios run against the same image:
#
#   routing   the image's own /etc/caddy/Caddyfile, configured by NOTHING but
#             environment variables. Proves routing across two fleets, the
#             cross-fleet claim rule, the denied fleet API paths, the
#             scafo.fleet request variable, http_redirect off for one fleet
#             only, and the on-demand permission refusing a name no fleet
#             serves.
#
#   acme      the same image and the same environment, with a Caddyfile that
#             adds one line -- acme_ca -- because Caddy's DEFAULT certificate
#             authority is not settable from the environment and this must
#             never reach a public one. Proves that a certificate issued for
#             one fleet's host lands in THAT fleet's storage and in no other.
#
# NOTHING HERE TALKS TO A PUBLIC CA. Every Caddy container runs with
# SSL_CERT_FILE pointing at a trust store that holds either nothing at all or
# Pebble's root alone, so a request to any other certificate authority fails at
# the TLS handshake rather than arriving. The routing scenario additionally
# never follows a redirect to HTTPS, so no handshake for a name the permission
# module ALLOWS ever happens there.
#
# Usage: internal/e2e/run.sh [routing|acme|all]
set -euo pipefail

SCENARIO="${1:-all}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
NETWORK="scafo-e2e"
IMAGE="scafo-caddy:multi-fleet"
STUB_IMAGE="scafo-e2e-stub:latest"
PEBBLE_IMAGE="ghcr.io/letsencrypt/pebble:latest"
CHALLTEST_IMAGE="ghcr.io/letsencrypt/pebble-challtestsrv:latest"
CURL_IMAGE="curlimages/curl:latest"

WORK="$(mktemp -d)"
FAILURES=0
CHECKS=0
CADDY_IP=""

cleanup() {
	docker rm -f e2e-caddy e2e-stub-a e2e-stub-b e2e-pebble e2e-challtestsrv >/dev/null 2>&1 || true
	docker network rm "$NETWORK" >/dev/null 2>&1 || true
	rm -rf "$WORK"
}
trap cleanup EXIT

say() { printf '\n== %s\n' "$*"; }

check() {
	CHECKS=$((CHECKS + 1))
	if [ "$2" = "$3" ]; then
		printf '  ok    %s\n' "$1"
	else
		FAILURES=$((FAILURES + 1))
		printf '  FAIL  %s\n        want: %s\n        got:  %s\n' "$1" "$2" "$3"
	fi
}

contains() {
	CHECKS=$((CHECKS + 1))
	case "$3" in
	*"$2"*) printf '  ok    %s\n' "$1" ;;
	*)
		FAILURES=$((FAILURES + 1))
		printf '  FAIL  %s\n        want it to contain: %s\n        got:  %s\n' "$1" "$2" "$3"
		;;
	esac
}

lacks() {
	CHECKS=$((CHECKS + 1))
	case "$3" in
	*"$2"*)
		FAILURES=$((FAILURES + 1))
		printf '  FAIL  %s\n        want it NOT to contain: %s\n        got:  %s\n' "$1" "$2" "$3"
		;;
	*) printf '  ok    %s\n' "$1" ;;
	esac
}

# curl_in runs curl on the test network, resolving every test name to Caddy.
# It never follows a redirect on its own: a redirect to HTTPS is an answer to
# assert, not a step to take.
curl_in() {
	docker run --rm --network "$NETWORK" \
		--add-host "app.a.example:$CADDY_IP" \
		--add-host "app.b.example:$CADDY_IP" \
		--add-host "shared.example:$CADDY_IP" \
		--add-host "a.example:$CADDY_IP" \
		--add-host "x.wild-a.example:$CADDY_IP" \
		--add-host "stranger.example:$CADDY_IP" \
		"$CURL_IMAGE" -sS --max-time 30 "$@" 2>&1
}

ROUTES_A='{"v":1,"routes":[
  {"host":"app.a.example","application":"pa","upstreams":[{"node":"na","address":"e2e-stub-a:8080"}]},
  {"host":"shared.example","application":"pa","upstreams":[{"node":"na","address":"e2e-stub-a:8080"}]},
  {"host":"*.wild-a.example","application":"pa","upstreams":[{"node":"na","address":"e2e-stub-a:8080"}]}
],
"api":{"hosts":["a.example"],"upstreams":[{"node":"na","address":"e2e-stub-a:7400"}],
       "denied":["/v1/storage","/v1/ingress/routes"]},
"wildcards":["wild-a.example"]}'

ROUTES_B='{"v":1,"routes":[
  {"host":"app.b.example","application":"pb","upstreams":[{"node":"nb","address":"e2e-stub-b:8080"}]},
  {"host":"shared.example","application":"pb","upstreams":[{"node":"nb","address":"e2e-stub-b:8080"}]}
]}'

build() {
	say "building the image and the node stand-in"
	docker build -t "$IMAGE" "$REPO_ROOT" >/dev/null
	docker build -t "$STUB_IMAGE" -f "$REPO_ROOT/internal/e2e/Dockerfile.stub" "$REPO_ROOT" >/dev/null
	docker pull -q "$CURL_IMAGE" >/dev/null
}

start_nodes() {
	docker network create "$NETWORK" >/dev/null
	docker run -d --name e2e-stub-a --network "$NETWORK" \
		-e FLEET=a -e TOKEN=token-a -e ROUTES="$ROUTES_A" "$STUB_IMAGE" >/dev/null
	docker run -d --name e2e-stub-b --network "$NETWORK" \
		-e FLEET=b -e TOKEN=token-b -e ROUTES="$ROUTES_B" "$STUB_IMAGE" >/dev/null
}

start_caddy() {
	docker rm -f e2e-caddy >/dev/null 2>&1 || true
	docker run -d --name e2e-caddy --network "$NETWORK" \
		-e SCAFO_FLEET_A_ENDPOINT=http://e2e-stub-a:7400 \
		-e SCAFO_FLEET_A_TOKEN=token-a \
		-e SCAFO_FLEET_A_POLL_INTERVAL=1s \
		-e SCAFO_FLEET_B_ENDPOINT=http://e2e-stub-b:7400 \
		-e SCAFO_FLEET_B_TOKEN=token-b \
		-e SCAFO_FLEET_B_POLL_INTERVAL=1s \
		-e SCAFO_FLEET_B_HTTP_REDIRECT=off \
		"$@" "$IMAGE" >/dev/null
	CADDY_IP="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' e2e-caddy)"
	for _ in $(seq 1 60); do
		docker logs e2e-caddy 2>&1 | grep -q "serving initial configuration" && break
		sleep 0.5
	done
	sleep 2
}

# An empty trust store: nothing this Caddy dials over TLS can be trusted, which
# is the structural guarantee that no certificate authority is reached.
empty_trust_store() {
	: >"$WORK/empty-ca.pem"
}

scenario_routing() {
	say "scenario: routing, with the image's own Caddyfile and nothing but environment variables"
	empty_trust_store
	start_nodes
	start_caddy \
		-e SSL_CERT_FILE=/empty-ca.pem \
		-v "$WORK/empty-ca.pem:/empty-ca.pem:ro"

	say "http_redirect: off for fleet b only"
	check "fleet a's host is redirected to HTTPS" "308" \
		"$(curl_in -o /dev/null -w '%{http_code}' http://app.a.example/hello)"
	contains "and the redirect names the same host over https" \
		"location: https://app.a.example/hello" \
		"$(curl_in -i -o - http://app.a.example/hello | tr '[:upper:]' '[:lower:]')"
	check "fleet b's host is served over plain HTTP" "200" \
		"$(curl_in -o /dev/null -w '%{http_code}' http://app.b.example/hello)"
	contains "and its body comes from fleet b's own backend" \
		"fleet=b host=app.b.example" "$(curl_in http://app.b.example/hello)"
	check "a host no fleet serves is redirected like everything else" "308" \
		"$(curl_in -o /dev/null -w '%{http_code}' http://stranger.example/)"

	say "the on-demand permission refuses a name no fleet serves"
	local handshake
	handshake="$(curl_in -k -o /dev/null -w '%{http_code}' https://stranger.example/ || true)"
	lacks "the handshake does not succeed" "200" "$handshake"
	contains "and the permission module says why" \
		"no fleet serves" \
		"$(docker logs e2e-caddy 2>&1 | grep -i 'stranger.example' | tail -1)"

	say "routing across both fleets (both fleets plain, so nothing reaches a CA)"
	start_caddy \
		-e SCAFO_FLEET_A_HTTP_REDIRECT=off \
		-e SSL_CERT_FILE=/empty-ca.pem \
		-v "$WORK/empty-ca.pem:/empty-ca.pem:ro"

	contains "fleet a serves its own host" \
		"fleet=a host=app.a.example" "$(curl_in http://app.a.example/hello)"
	contains "fleet b serves its own host" \
		"fleet=b host=app.b.example" "$(curl_in http://app.b.example/hello)"
	contains "a wildcard route covers a name under its base" \
		"fleet=a host=x.wild-a.example" "$(curl_in http://x.wild-a.example/hello)"
	contains "a host both fleets publish goes to the one declared first" \
		"fleet=a host=shared.example" "$(curl_in http://shared.example/)"
	check "the conflict is logged once, not once per poll" "1" \
		"$(docker logs e2e-caddy 2>&1 | grep -c 'two fleets publish the same host' || true)"
	contains "and the log names the host and the fleets claiming it" \
		"shared.example" \
		"$(docker logs e2e-caddy 2>&1 | grep 'two fleets publish the same host' | tail -1)"

	say "the fleet API surface"
	contains "a fleet API host is proxied to its own daemon" \
		'"fleet":"a"' "$(curl_in http://a.example/_debug/fleet)"
	contains "a denied storage path is answered by the ingress" \
		"Nothing is served here" "$(curl_in http://a.example/v1/storage/list)"
	contains "and so is the routing document" \
		"Nothing is served here" "$(curl_in http://a.example/v1/ingress/routes)"
	# Proxied, not denied: the answer comes from the daemon behind the ingress
	# (a 404 of its own, since the stand-in has no such route) rather than
	# from the ingress's own refusal page.
	lacks "while a path that merely shares a prefix is proxied, not denied" \
		"Nothing is served here" "$(curl_in http://a.example/v1/storagex)"

	say "the scafo.fleet request variable"
	# Proved by handing it to a handler: the variable is set before
	# reverse_proxy dials, so a response header built from it names the fleet
	# that answered. This is the one check that needs a Caddyfile of its own.
	awk '/^\t\tdynamic scafo$/ { print; print "\t\theader_down X-Scafo-Fleet {http.vars.scafo.fleet}"; next } { print }' \
		"$REPO_ROOT/image/Caddyfile" >"$WORK/Caddyfile-fleetvar"
	start_caddy \
		-e SCAFO_FLEET_A_HTTP_REDIRECT=off \
		-e SSL_CERT_FILE=/empty-ca.pem \
		-v "$WORK/empty-ca.pem:/empty-ca.pem:ro" \
		-v "$WORK/Caddyfile-fleetvar:/etc/caddy/Caddyfile:ro"
	contains "scafo.fleet names fleet a" "x-scafo-fleet: a" \
		"$(curl_in -i -o - http://app.a.example/ | tr '[:upper:]' '[:lower:]')"
	contains "scafo.fleet names fleet b" "x-scafo-fleet: b" \
		"$(curl_in -i -o - http://app.b.example/ | tr '[:upper:]' '[:lower:]')"

	say "no certificate authority was contacted"
	lacks "not Let's Encrypt" "acme-v02.api.letsencrypt.org" "$(docker logs e2e-caddy 2>&1)"
	lacks "not ZeroSSL" "acme.zerossl.com" "$(docker logs e2e-caddy 2>&1)"

	docker rm -f e2e-caddy e2e-stub-a e2e-stub-b >/dev/null
	docker network rm "$NETWORK" >/dev/null
}

scenario_acme() {
	say "scenario: a certificate from a local ACME server lands in one fleet's storage only"
	docker pull -q "$PEBBLE_IMAGE" >/dev/null
	docker pull -q "$CHALLTEST_IMAGE" >/dev/null

	start_nodes

	docker run -d --name e2e-challtestsrv --network "$NETWORK" \
		--network-alias challtestsrv "$CHALLTEST_IMAGE" \
		-dnsserver ':8053' -doh '' -http01 '' -https01 '' -tlsalpn01 '' -management ':8055' -defaultIPv6 '' >/dev/null

	local challtest_ip
	challtest_ip="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' e2e-challtestsrv)"

	docker run -d --name e2e-pebble --network "$NETWORK" --network-alias pebble \
		-v "$REPO_ROOT/internal/e2e/pebble-config.json:/test/config/pebble-e2e.json:ro" \
		-e PEBBLE_VA_NOSLEEP=1 -e PEBBLE_WFE_NONCEREJECT=0 \
		"$PEBBLE_IMAGE" -config /test/config/pebble-e2e.json -dnsserver "$challtest_ip:8053" >/dev/null

	sleep 3
	# Pebble's own root, so that Caddy trusts Pebble and NOTHING ELSE.
	docker cp e2e-pebble:/test/certs/pebble.minica.pem "$WORK/pebble.minica.pem" >/dev/null

	# The image's Caddyfile plus the default certificate authority, which is
	# not settable from the environment and must not be a public one here.
	awk '/^\tscafo \{$/ { print "\tacme_ca {$SCAFO_ACME_CA}"; print "\temail {$SCAFO_ACME_EMAIL}" } { print }' \
		"$REPO_ROOT/image/Caddyfile" >"$WORK/Caddyfile-acme"

	start_caddy \
		-e SCAFO_ACME_CA=https://pebble:14000/dir \
		-e SCAFO_ACME_EMAIL=ops@example.test \
		-e SSL_CERT_FILE=/pebble.minica.pem \
		-v "$WORK/pebble.minica.pem:/pebble.minica.pem:ro" \
		-v "$WORK/Caddyfile-acme:/etc/caddy/Caddyfile:ro"

	# Point the challenge validator's DNS at Caddy, so Pebble's HTTP-01
	# request for the test names arrives here.
	docker run --rm --network "$NETWORK" "$CURL_IMAGE" -sS -X POST \
		-d "{\"ip\":\"$CADDY_IP\"}" "http://challtestsrv:8055/set-default-ipv4" >/dev/null

	say "issuing, on demand, for one fleet's host"
	check "a TLS request for fleet a's host is served" "200" \
		"$(curl_in -k -o /dev/null -w '%{http_code}' --max-time 90 https://app.a.example/ || true)"
	sleep 3

	local keys_a keys_b
	keys_a="$(curl_in http://e2e-stub-a:7400/_debug/keys)"
	keys_b="$(curl_in http://e2e-stub-b:7400/_debug/keys)"
	printf '\n  fleet a holds: %s\n  fleet b holds: %s\n\n' "$keys_a" "$keys_b"

	say "where the certificate landed"
	contains "fleet a holds the certificate for its own host" "app.a.example.crt" "$keys_a"
	contains "fleet a holds its private key" "app.a.example.key" "$keys_a"
	lacks "fleet b holds nothing of fleet a's" "app.a.example" "$keys_b"
	lacks "the ACME account is in no fleet's storage" "/users/" "$keys_a"
	lacks "and not in fleet b's either" "/users/" "$keys_b"
	lacks "CertMagic's housekeeping is in no fleet's storage" "last_clean.json" "$keys_a"
	lacks "CertMagic's storage check is not in a fleet's storage" "rw_test" "$keys_a"

	say "the certificate came from the local CA and nowhere else"
	lacks "not Let's Encrypt" "acme-v02.api.letsencrypt.org" "$(docker logs e2e-caddy 2>&1)"
	contains "the order went to Pebble" "pebble:14000" \
		"$(docker logs e2e-caddy 2>&1 | grep -i 'certificate obtained\|acme' | head -40)"

	docker rm -f e2e-caddy e2e-stub-a e2e-stub-b e2e-pebble e2e-challtestsrv >/dev/null
	docker network rm "$NETWORK" >/dev/null
}

build
case "$SCENARIO" in
routing) scenario_routing ;;
acme) scenario_acme ;;
all)
	scenario_routing
	scenario_acme
	;;
*)
	echo "unknown scenario: $SCENARIO" >&2
	exit 2
	;;
esac

printf '\n%d checks, %d failed\n' "$CHECKS" "$FAILURES"
[ "$FAILURES" -eq 0 ]
