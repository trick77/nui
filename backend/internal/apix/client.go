package apix

import (
	"net/http"
	"net/netip"
	"strings"

	"github.com/trick77/ship/kit/ratelimit"
)

// ClientAddr is the caller's address. hops is the number of trusted proxies in front of the server, each appending
// the address it saw to X-Forwarded-For: the entry hops places from the right is the caller as the outermost trusted
// proxy saw it; with 0 (no proxy) it is the connection's peer. An entry that is no address, or a header with fewer
// entries than hops, falls back to the peer: a caller reaching the server around the proxy writes the header itself,
// and what it writes must never become a key or an address it chose freely. Zones are dropped, v4-mapped addresses
// unmapped. false only when not even the peer is an address (never on a real connection).
func ClientAddr(r *http.Request, hops int) (netip.Addr, bool) {
	if hops > 0 {
		if xff := strings.Join(r.Header.Values("X-Forwarded-For"), ","); strings.TrimSpace(xff) != "" {
			parts := strings.Split(xff, ",")
			if i := len(parts) - hops; i >= 0 {
				if a, ok := parseAddr(parts[i]); ok {
					return a, true
				}
			}
		}
	}
	return parseAddr(r.RemoteAddr)
}

// parseAddr reads an address, with or without a port or brackets.
func parseAddr(s string) (netip.Addr, bool) {
	s = strings.TrimSpace(s)
	a, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(s, "["), "]"))
	if err != nil {
		ap, perr := netip.ParseAddrPort(s)
		if perr != nil {
			return netip.Addr{}, false
		}
		a = ap.Addr()
	}
	return a.WithZone("").Unmap(), true
}

// ClientIP is ClientAddr as text for the access log, "" without one.
func ClientIP(hops int) func(*http.Request) string {
	return func(r *http.Request) string {
		if a, ok := ClientAddr(r, hops); ok {
			return a.String()
		}
		return ""
	}
}

// noAddr is the one bucket of requests without any address (none on a real connection).
const noAddr = "-"

// ClientKey is a rate-limit key for the caller's network: ratelimit.PrefixKey(v4, v6) over ClientAddr, so always
// an address or a prefix, never a caller-chosen string. PrefixKey panics on a bit count out of range here, at
// construction.
func ClientKey(hops, v4, v6 int) func(*http.Request) string {
	key := ratelimit.PrefixKey(v4, v6)
	return func(r *http.Request) string {
		if a, ok := ClientAddr(r, hops); ok {
			return key(a.String())
		}
		return noAddr
	}
}
