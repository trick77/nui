package refdata

import (
	"encoding/binary"
	"net/netip"
	"slices"
)

// PrefixTable maps CIDR prefixes to values with longest-prefix match. One sorted array of network addresses per
// prefix length: a lookup masks the address to each stored length, longest first, and binary-searches that level.
// Prefixes of different owners nest (another AS's /24 inside a /16), which is why the levels stay apart instead of
// being flattened into ranges. Eight to twenty bytes a prefix: the full routing table (about a million prefixes)
// fits in tens of MB. Fill with Insert, then Build once; Lookup is safe for concurrent use after Build.
type PrefixTable struct {
	v4     [33]level4
	v6     [129]level6
	lens4  []int // non-empty v4 levels, longest first
	lens6  []int
	length int
}

type level4 struct {
	keys []uint32
	vals []int32
}

type u128 struct{ hi, lo uint64 }

type level6 struct {
	keys []u128
	vals []int32
}

// Insert adds prefix → v; a later insert of the same prefix wins at Build. Invalid prefixes are ignored.
func (t *PrefixTable) Insert(p netip.Prefix, v int32) {
	if !p.IsValid() {
		return
	}
	p = p.Masked()
	if p.Addr().Is4() {
		l := &t.v4[p.Bits()]
		l.keys = append(l.keys, key4(p.Addr()))
		l.vals = append(l.vals, v)
		return
	}
	l := &t.v6[p.Bits()]
	l.keys = append(l.keys, key6(p.Addr()))
	l.vals = append(l.vals, v)
}

// Build sorts every level and drops duplicate prefixes, keeping the last value inserted.
func (t *PrefixTable) Build() {
	t.lens4, t.lens6, t.length = nil, nil, 0
	for bits := 32; bits >= 0; bits-- {
		l := &t.v4[bits]
		if len(l.keys) == 0 {
			continue
		}
		idx := sortedIndex(len(l.keys), func(a, b int) int { return cmpU32(l.keys[a], l.keys[b]) })
		keys, vals := make([]uint32, 0, len(idx)), make([]int32, 0, len(idx))
		for _, i := range idx {
			if n := len(keys); n > 0 && keys[n-1] == l.keys[i] {
				vals[n-1] = l.vals[i]
				continue
			}
			keys, vals = append(keys, l.keys[i]), append(vals, l.vals[i])
		}
		l.keys, l.vals = keys, vals
		t.lens4 = append(t.lens4, bits)
		t.length += len(keys)
	}
	for bits := 128; bits >= 0; bits-- {
		l := &t.v6[bits]
		if len(l.keys) == 0 {
			continue
		}
		idx := sortedIndex(len(l.keys), func(a, b int) int { return cmp128(l.keys[a], l.keys[b]) })
		keys, vals := make([]u128, 0, len(idx)), make([]int32, 0, len(idx))
		for _, i := range idx {
			if n := len(keys); n > 0 && keys[n-1] == l.keys[i] {
				vals[n-1] = l.vals[i]
				continue
			}
			keys, vals = append(keys, l.keys[i]), append(vals, l.vals[i])
		}
		l.keys, l.vals = keys, vals
		t.lens6 = append(t.lens6, bits)
		t.length += len(keys)
	}
}

// sortedIndex is 0..n-1 stably sorted by cmp, so the last insert of equal keys stays last.
func sortedIndex(n int, cmp func(a, b int) int) []int {
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	slices.SortStableFunc(idx, cmp)
	return idx
}

// Len is the number of distinct prefixes after Build.
func (t *PrefixTable) Len() int { return t.length }

// Lookup is the value of the longest prefix containing a.
func (t *PrefixTable) Lookup(a netip.Addr) (int32, bool) {
	if !a.IsValid() {
		return 0, false
	}
	a = a.Unmap()
	if a.Is4() {
		k := key4(a)
		for _, bits := range t.lens4 {
			l := &t.v4[bits]
			m := k & mask32(bits)
			if i, ok := slices.BinarySearch(l.keys, m); ok {
				return l.vals[i], true
			}
		}
		return 0, false
	}
	k := key6(a)
	for _, bits := range t.lens6 {
		l := &t.v6[bits]
		m := mask128(k, bits)
		if i, ok := slices.BinarySearchFunc(l.keys, m, cmp128); ok {
			return l.vals[i], true
		}
	}
	return 0, false
}

func key4(a netip.Addr) uint32 {
	b := a.As4()
	return binary.BigEndian.Uint32(b[:])
}

func key6(a netip.Addr) u128 {
	b := a.As16()
	return u128{binary.BigEndian.Uint64(b[:8]), binary.BigEndian.Uint64(b[8:])}
}

func mask32(bits int) uint32 {
	if bits == 0 {
		return 0
	}
	return ^uint32(0) << (32 - bits)
}

func mask128(k u128, bits int) u128 {
	switch {
	case bits == 0:
		return u128{}
	case bits <= 64:
		return u128{k.hi & (^uint64(0) << (64 - bits)), 0}
	case bits == 128:
		return k
	default:
		return u128{k.hi, k.lo & (^uint64(0) << (128 - bits))}
	}
}

func cmpU32(a, b uint32) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmp128(a, b u128) int {
	switch {
	case a.hi < b.hi:
		return -1
	case a.hi > b.hi:
		return 1
	case a.lo < b.lo:
		return -1
	case a.lo > b.lo:
		return 1
	}
	return 0
}
