// Package netaddr holds the address predicates that more than one package
// has to agree on. It exists because both the admin listener and config
// validation need the same answer to "does this bind reach off the machine?",
// and config cannot import admin — admin already imports config, so that
// direction is a cycle. One implementation, two callers, no chance of the
// two drifting into subtly different notions of "local".
package netaddr

import (
	"net"
	"strings"
)

// IsLoopback reports whether addr binds only to the local machine. It accepts
// either a bare host or a host:port, with or without brackets around an IPv6
// literal. Anything that does not resolve to a loopback IP — a hostname, the
// unspecified address 0.0.0.0 or ::, an empty string — is reported as not
// loopback, so callers gating on it fail closed.
func IsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
