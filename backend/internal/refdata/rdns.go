package refdata

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"time"
)

// Resolver is the DNS the verifier asks; *net.Resolver in production.
type Resolver interface {
	LookupAddr(ctx context.Context, addr string) ([]string, error)
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// Verifier confirms a non-authoritative bot-list hit by forward-confirmed reverse DNS: the address's PTR name must
// match the provider's rDNS glob and resolve back to the address. Without the forward step anyone controlling the
// reverse zone of an address inside a listed range could name it googlebot. Answers are cached for a day, bounded.
type Verifier struct {
	r       Resolver
	timeout time.Duration
	mu      sync.Mutex
	cache   map[string]verdict
}

type verdict struct {
	host string // the confirmed PTR name, "" when none confirmed
	at   time.Time
}

// maxVerdicts bounds the cache; at the bound it is cleared (cheap, and the classifier re-asks at most daily).
const maxVerdicts = 50_000

// NewVerifier asks r, each lookup bounded by timeout.
func NewVerifier(r Resolver, timeout time.Duration) *Verifier {
	if r == nil {
		r = net.DefaultResolver
	}
	return &Verifier{r: r, timeout: timeout, cache: map[string]verdict{}}
}

// Confirm reports whether ip's forward-confirmed PTR name matches one of patterns.
func (v *Verifier) Confirm(ctx context.Context, ip string, patterns []string) bool {
	if len(patterns) == 0 {
		return false
	}
	host := v.confirmedHost(ctx, ip)
	return host != "" && MatchRdnsGlob(host, patterns)
}

// confirmedHost is ip's PTR name that resolves back to ip, "" when there is none.
func (v *Verifier) confirmedHost(ctx context.Context, ip string) string {
	v.mu.Lock()
	if c, ok := v.cache[ip]; ok && time.Since(c.at) < 24*time.Hour {
		v.mu.Unlock()
		return c.host
	}
	v.mu.Unlock()
	host := v.lookup(ctx, ip)
	v.mu.Lock()
	if len(v.cache) >= maxVerdicts {
		clear(v.cache)
	}
	v.cache[ip] = verdict{host: host, at: time.Now()}
	v.mu.Unlock()
	return host
}

func (v *Verifier) lookup(ctx context.Context, ip string) string {
	want, err := netip.ParseAddr(ip)
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()
	names, err := v.r.LookupAddr(ctx, ip)
	if err != nil {
		return ""
	}
	for _, name := range names {
		addrs, err := v.r.LookupHost(ctx, name)
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if got, err := netip.ParseAddr(a); err == nil && got.Unmap() == want.Unmap() {
				return name
			}
		}
	}
	return ""
}
