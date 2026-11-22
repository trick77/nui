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
// reverse zone of an address inside a listed range could name it googlebot.
//
// Lookups never run on the classification path: Check answers from the cache and queues a miss for Run, one lookup
// at a time. Any client can claim to be a crawler, so lookups are bounded per pass, per network a day (a /24, an IPv6
// /48: rotating addresses or /64s inside one costs the claimant nothing, a new /48 does) and by the queue; past the
// network's budget a claim stays unconfirmed. Verdicts are cached per address for a day, bounded.
type Verifier struct {
	r       Resolver
	timeout time.Duration
	now     func() time.Time
	queue   chan string

	mu       sync.Mutex
	cache    map[string]verdict
	queued   map[string]bool
	budget   map[string]int // network → lookups queued today
	day      int64
	passLeft int
}

type verdict struct {
	host string // the confirmed PTR name, "" when none confirmed
	at   time.Time
}

const (
	// maxVerdicts bounds the cache and the budget map; at the bound they are cleared (cheap, and the classifier
	// re-asks at most daily).
	maxVerdicts = 50_000
	// LookupsPerPass bounds the lookups one classification pass queues; lookupsPerNetwork those one network gets a
	// day; queueSize the lookups waiting.
	LookupsPerPass    = 32
	lookupsPerNetwork = 16
	queueSize         = 64
)

// NewVerifier asks r, each lookup bounded by timeout.
func NewVerifier(r Resolver, timeout time.Duration) *Verifier {
	if r == nil {
		r = net.DefaultResolver
	}
	return &Verifier{r: r, timeout: timeout, now: time.Now, queue: make(chan string, queueSize),
		cache: map[string]verdict{}, queued: map[string]bool{}, budget: map[string]int{}, passLeft: LookupsPerPass}
}

// StartPass gives a new classification pass its lookup bound.
func (v *Verifier) StartPass() {
	v.mu.Lock()
	v.passLeft = LookupsPerPass
	v.mu.Unlock()
}

// Check reports whether ip's forward-confirmed PTR name matches one of patterns, from the cache. Pending is true when
// the answer is not known yet: a lookup is queued (or will be on a later pass), ask again then. A claim past its
// network's daily budget is answered unconfirmed, not pending.
func (v *Verifier) Check(ip string, patterns []string) (confirmed, pending bool) {
	if len(patterns) == 0 {
		return false, false
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false, false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if c, ok := v.cache[ip]; ok && v.now().Sub(c.at) < 24*time.Hour {
		return c.host != "" && MatchRdnsGlob(c.host, patterns), false
	}
	if v.queued[ip] {
		return false, true
	}
	if day := v.now().Unix() / 86400; day != v.day || len(v.budget) >= maxVerdicts {
		v.day = day
		clear(v.budget)
	}
	network := networkOf(addr)
	switch {
	case v.budget[network] >= lookupsPerNetwork:
		return false, false
	case v.passLeft <= 0:
		return false, true
	}
	select {
	case v.queue <- ip:
		v.queued[ip] = true
		v.budget[network]++
		v.passLeft--
	default: // full: a later pass asks again
	}
	return false, true
}

// networkOf is the budget's key: the address's /24 or /48.
func networkOf(a netip.Addr) string {
	a = a.Unmap()
	bits := 48
	if a.Is4() {
		bits = 24
	}
	p, _ := a.Prefix(bits)
	return p.String()
}

// Run does the queued lookups, one at a time, until ctx ends.
func (v *Verifier) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ip := <-v.queue:
			v.answer(ctx, ip)
		}
	}
}

// Work does the lookups queued now and returns (tests, and a caller without Run).
func (v *Verifier) Work(ctx context.Context) {
	for {
		select {
		case ip := <-v.queue:
			v.answer(ctx, ip)
		default:
			return
		}
	}
}

func (v *Verifier) answer(ctx context.Context, ip string) {
	host := v.lookup(ctx, ip)
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.queued, ip)
	if ctx.Err() != nil {
		return // cut short, not an answer
	}
	if len(v.cache) >= maxVerdicts {
		clear(v.cache)
	}
	v.cache[ip] = verdict{host: host, at: v.now()}
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
