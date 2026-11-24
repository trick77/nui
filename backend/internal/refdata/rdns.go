package refdata

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
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
	hosts   []string // the confirmed PTR names
	unnamed bool     // the address has no PTR name at all
	failed  bool     // the lookup failed: nothing is known, ask again after failRetry
	partial bool     // beside hosts, names were left unchecked: no match refutes nothing
	at      time.Time
}

// Verdict is what the verifier knows about an address's claim to be a provider's.
type Verdict int

const (
	// Unverified means no lookup answers it now (the network's daily lookup budget is spent, the provider names no
	// rDNS pattern, no address, more names than are looked up); nothing is known either way.
	Unverified Verdict = iota
	// Pending means a lookup is queued, or will be on a later pass; ask again then.
	Pending
	// Failed means the last lookup failed and is retried after failRetry; nothing is known either way.
	Failed
	// Confirmed means the address's forward-confirmed PTR name matches the provider's rDNS glob.
	Confirmed
	// Refuted means a completed lookup found PTR names, none of them forward-confirmed and matching it.
	Refuted
	// Unnamed means a completed lookup found no PTR name: Meta's crawlers send from 57.141.0.0/16, which has none.
	Unnamed
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
	// maxNames bounds the PTR names of one address looked up: whoever owns the reverse zone sets their number.
	maxNames = 8
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

// Check reports what is known of ip's forward-confirmed PTR names against patterns, from the cache, and queues a
// lookup when nothing is: Pending then, ask again later. Only a completed lookup that found names refutes; a claim past
// its network's daily budget is Unverified. For a client that claims to be the provider inside its ranges.
func (v *Verifier) Check(ip string, patterns []string) Verdict { return v.check(ip, patterns, anyRun) }

// CheckClose is Check for any other client: the name must be right under the pattern (oneLabel).
func (v *Verifier) CheckClose(ip string, patterns []string) Verdict {
	return v.check(ip, patterns, oneLabel)
}

func (v *Verifier) check(ip string, patterns []string, star string) Verdict {
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
		case age < 24*time.Hour && slices.ContainsFunc(c.hosts, func(h string) bool { return matchRdns(h, patterns, star) }):
			return Confirmed
		case c.failed && age < failRetry:
			return Failed
		case c.partial && !c.failed && age < 24*time.Hour:
			return Unverified
		case c.unnamed && age < 24*time.Hour:
			return Unnamed
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
	res := v.lookup(ctx, ip)
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.queued, ip)
	if ctx.Err() != nil {
		return // cut short, not an answer
	}
	if len(v.cache) >= maxVerdicts {
		clear(v.cache)
	}
	res.at = v.now()
	v.cache[ip] = res
}

// lookup is what DNS says of ip, every PTR name forward-confirmed (an address may have several, the provider's among
// them), at once: a slow name's lookup must not take the others' time. Failed is no answer: the reverse lookup, or
// with no name confirmed a forward one, failed otherwise than with "no such name".
func (v *Verifier) lookup(ctx context.Context, ip string) verdict {
	want, err := netip.ParseAddr(ip)
	if err != nil {
		return verdict{}
	}
	ctx, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()
	names, err := v.r.LookupAddr(ctx, ip)
	switch {
	case err != nil && !notFound(err):
		return verdict{failed: true}
	case len(names) == 0:
		return verdict{unnamed: true}
	}
	res := verdict{partial: len(names) > maxNames}
	names = names[:min(len(names), maxNames)]
	confirmed, failed := make([]bool, len(names)), make([]bool, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Go(func() {
			addrs, err := v.r.LookupHost(ctx, name)
			failed[i] = err != nil && !notFound(err)
			confirmed[i] = slices.ContainsFunc(addrs, func(a string) bool {
				got, err := netip.ParseAddr(a)
				return err == nil && got.Unmap() == want.Unmap()
			})
		})
	}
	wg.Wait()
	for i, name := range names {
		if confirmed[i] {
			res.hosts = append(res.hosts, name)
		}
	}
	if slices.Contains(failed, true) {
		res.partial, res.failed = true, len(res.hosts) == 0
	}
	return res
}

// notFound reports a definite "no such name": an answer, unlike a timeout or a server failure.
func notFound(err error) bool {
	var de *net.DNSError
	return errors.As(err, &de) && de.IsNotFound
}
