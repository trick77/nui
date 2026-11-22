package refdata

import (
	"context"
	"errors"
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
// network's budget a claim stays unverified. Answers are cached per address for a day, bounded; a failed lookup (a
// timeout, a server failure) is no answer and is retried after failRetry.
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
	host   string // the confirmed PTR name, "" when none confirmed
	failed bool   // the lookup failed: nothing is known, ask again after failRetry
	at     time.Time
}

// Verdict is what the verifier knows about an address's claim to be a provider's.
type Verdict int

const (
	// Unverified means no lookup answers it now (the network's daily lookup budget is spent, the provider names no
	// rDNS pattern, no address); nothing is known either way.
	Unverified Verdict = iota
	// Pending means a lookup is queued, or will be on a later pass; ask again then.
	Pending
	// Failed means the last lookup failed and is retried after failRetry; nothing is known either way.
	Failed
	// Confirmed means the address's forward-confirmed PTR name matches the provider's rDNS glob.
	Confirmed
	// Refuted means a completed lookup found no forward-confirmed PTR name matching it.
	Refuted
)

const (
	// failRetry is how long a failed lookup stands before the next ask queues it again.
	failRetry = 10 * time.Minute
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

// Check reports what is known of ip's forward-confirmed PTR name against patterns, from the cache, and queues a
// lookup when nothing is: Pending then, ask again later. Only a completed lookup refutes; a claim past its network's
// daily budget is Unverified.
func (v *Verifier) Check(ip string, patterns []string) Verdict {
	if len(patterns) == 0 {
		return Unverified
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return Unverified
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if c, ok := v.cache[ip]; ok {
		age := v.now().Sub(c.at)
		switch {
		case c.failed && age < failRetry:
			return Failed
		case !c.failed && age < 24*time.Hour && c.host != "" && MatchRdnsGlob(c.host, patterns):
			return Confirmed
		case !c.failed && age < 24*time.Hour:
			return Refuted
		}
	}
	if v.queued[ip] {
		return Pending
	}
	if day := v.now().Unix() / 86400; day != v.day || len(v.budget) >= maxVerdicts {
		v.day = day
		clear(v.budget)
	}
	network := networkOf(addr)
	switch {
	case v.budget[network] >= lookupsPerNetwork:
		return Unverified
	case v.passLeft <= 0:
		return Pending
	}
	select {
	case v.queue <- ip:
		v.queued[ip] = true
		v.budget[network]++
		v.passLeft--
	default: // full: a later pass asks again
	}
	return Pending
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
	host, err := v.lookup(ctx, ip)
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.queued, ip)
	if ctx.Err() != nil {
		return // cut short, not an answer
	}
	if len(v.cache) >= maxVerdicts {
		clear(v.cache)
	}
	v.cache[ip] = verdict{host: host, failed: err != nil, at: v.now()}
}

// lookup is ip's forward-confirmed PTR name, "" for none. An error means no answer: the reverse lookup or a forward
// one that might have confirmed failed otherwise than with "no such name".
func (v *Verifier) lookup(ctx context.Context, ip string) (string, error) {
	want, err := netip.ParseAddr(ip)
	if err != nil {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()
	names, err := v.r.LookupAddr(ctx, ip)
	if err != nil {
		if notFound(err) {
			return "", nil
		}
		return "", err
	}
	var failed error
	for _, name := range names {
		addrs, err := v.r.LookupHost(ctx, name)
		if err != nil {
			if !notFound(err) {
				failed = err
			}
			continue
		}
		for _, a := range addrs {
			if got, err := netip.ParseAddr(a); err == nil && got.Unmap() == want.Unmap() {
				return name, nil
			}
		}
	}
	return "", failed
}

// notFound reports a definite "no such name": an answer, unlike a timeout or a server failure.
func notFound(err error) bool {
	var de *net.DNSError
	return errors.As(err, &de) && de.IsNotFound
}
