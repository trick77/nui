package apix

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientAddr(t *testing.T) {
	for _, tt := range []struct {
		name, peer string
		xff        []string
		hops       int
		want       string
	}{
		{"proxied", "10.0.0.2:1234", []string{"203.0.113.9"}, 1, "203.0.113.9"},
		{"spoof ahead of the proxy's entry", "10.0.0.2:1234", []string{"198.51.100.1, 203.0.113.9"}, 1, "203.0.113.9"},
		{"two proxies", "10.0.0.2:1234", []string{"203.0.113.9, 10.0.0.9"}, 2, "203.0.113.9"},
		{"headers joined", "10.0.0.2:1234", []string{"203.0.113.9", "10.0.0.9"}, 2, "203.0.113.9"},
		{"port and brackets", "10.0.0.2:1234", []string{"[2001:db8::1]:443"}, 1, "2001:db8::1"},
		{"zone and v4-mapped", "10.0.0.2:1234", []string{"fe80::1%eth0"}, 1, "fe80::1"},
		{"v4-mapped", "10.0.0.2:1234", []string{"::ffff:203.0.113.9"}, 1, "203.0.113.9"},
		// Around the proxy the caller writes the header: what is no address is never taken.
		{"not an address", "198.51.100.7:1234", []string{"owner-" + strings.Repeat("x", 500)}, 1, "198.51.100.7"},
		{"fewer entries than hops", "198.51.100.7:1234", []string{"203.0.113.9"}, 2, "198.51.100.7"},
		{"no header", "198.51.100.7:1234", nil, 1, "198.51.100.7"},
		{"no proxy: the header ignored", "198.51.100.7:1234", []string{"203.0.113.9"}, 0, "198.51.100.7"},
		{"peer without a port", "198.51.100.7", nil, 0, "198.51.100.7"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = tt.peer
		for _, v := range tt.xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		a, ok := ClientAddr(r, tt.hops)
		if !ok || a.String() != tt.want || ClientIP(tt.hops)(r) != tt.want {
			t.Errorf("%s: %v %v, want %s", tt.name, a, ok, tt.want)
		}
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "pipe"
	if _, ok := ClientAddr(r, 0); ok || ClientIP(0)(r) != "" || ClientKey(0, 32, 64)(r) != noAddr {
		t.Error("an address from a peer that has none")
	}
}

// A key is always an address or a prefix: a header's text never becomes one, so a caller around the proxy cannot
// pick fresh buckets or name the owner's.
func TestClientKey(t *testing.T) {
	key := func(hops int, peer, xff string, v4, v6 int) string {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = peer
		r.Header.Set("X-Forwarded-For", xff)
		return ClientKey(hops, v4, v6)(r)
	}
	for _, tt := range []struct {
		got, want string
	}{
		{key(1, "10.0.0.2:1", "203.0.113.9", 32, 64), "203.0.113.9"},
		{key(1, "10.0.0.2:1", "203.0.113.9", 24, 48), "203.0.113.0/24"},
		{key(1, "10.0.0.2:1", "2001:db8:1:2::7", 32, 64), "2001:db8:1:2::/64"},
		{key(1, "10.0.0.2:1", "2001:db8:1:2::7", 24, 48), "2001:db8:1::/48"},
		{key(1, "198.51.100.7:1", "guess-1", 32, 64), "198.51.100.7"},
		{key(1, "198.51.100.7:1", "guess-2", 32, 64), "198.51.100.7"},
		{key(0, "198.51.100.7:1", "203.0.113.9", 32, 64), "198.51.100.7"},
	} {
		if tt.got != tt.want {
			t.Errorf("key %q, want %q", tt.got, tt.want)
		}
	}
}
