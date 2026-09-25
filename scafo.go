// Package scafoingress is the root of the Caddy modules scafo relies on: the
// scafo app, which is where a fleet's endpoint and token are written down, and
// the four modules that read it -- TLS storage, dynamic upstreams, the
// on-demand TLS permission, and the plain-HTTP matcher.
//
// They live in one repository because they are one integration seen from
// several sides -- the same node daemons, the same fleets, the same error
// documents -- and are shipped together in one Caddy binary.
//
// This package holds no code of its own. It imports every module in the
// repository, which is what makes the whole set available by naming the root:
//
//	xcaddy build --with github.com/sandros94/scafo-ingress
//
// Naming a single package instead gets that module and the scafo app it reads,
// and nothing else:
//
//	xcaddy build --with github.com/sandros94/scafo-ingress/storage
package scafoingress

import (
	// Registers the scafo app and its global option. Every module below
	// imports it too; it is named here so that the app exists even in a build
	// that somehow reaches none of them.
	_ "github.com/sandros94/scafo-ingress/fleets"
	// Registers http.reverse_proxy.upstreams.scafo and
	// http.matchers.scafo_plain_http.
	_ "github.com/sandros94/scafo-ingress/ingress"
	// Registers tls.permission.scafo.
	_ "github.com/sandros94/scafo-ingress/permission"
	// Registers caddy.storage.scafo.
	_ "github.com/sandros94/scafo-ingress/storage"
)
