// Package scafoingress is the root of the Caddy modules scafo relies on: the
// TLS storage module caddy.storage.scafo, which keeps certificates in the
// bucket a scafo node daemon owns, and the dynamic upstreams module
// http.reverse_proxy.upstreams.scafo, which resolves a route's backends from
// that same daemon.
//
// They live in one repository because they are one integration seen from two
// sides -- the same node daemon, the same endpoint and token options, the same
// error documents -- and are shipped together in one Caddy binary.
//
// This package holds no code of its own. It imports every module in the
// repository, which is what makes the whole set available by naming the root:
//
//	xcaddy build --with github.com/sandros94/scafo-ingress
//
// A build that wants only one of them names it instead, and gets nothing else:
//
//	xcaddy build --with github.com/sandros94/scafo-ingress/storage
package scafoingress

import (
	// Registers http.reverse_proxy.upstreams.scafo.
	_ "github.com/sandros94/scafo-ingress/ingress"
	// Registers caddy.storage.scafo.
	_ "github.com/sandros94/scafo-ingress/storage"
)
