package dash

import "github.com/trick77/stakeout/backend/internal/agg"

// uniq counts distinct ids across pieces read from either source: an exact set of their hashes (agg.Hash, as the
// stored sketches use) until a stored sketch joins, then a sketch. The zero value is empty.
type uniq struct {
	set map[uint64]struct{}
	sk  *agg.Sketch
	n   int64 // after fix
}

// fix keeps the count and lets the ids go: nothing can be added or merged any more.
func (u *uniq) fix() { *u = uniq{n: u.count()} }

func (u *uniq) add(id string) { u.addHash(agg.Hash(id)) }

func (u *uniq) addHash(h uint64) {
	if u.sk != nil {
		u.sk.Add(h)
		return
	}
	if u.set == nil {
		u.set = map[uint64]struct{}{}
	}
	u.set[h] = struct{}{}
}

// toSketch turns the exact set into a sketch, once one has to be merged.
func (u *uniq) toSketch() {
	if u.sk != nil {
		return
	}
	u.sk = agg.NewSketch()
	for h := range u.set {
		u.sk.Add(h)
	}
	u.set = nil
}

// addSketch merges a stored sketch; nil or empty adds nothing.
func (u *uniq) addSketch(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	s, err := agg.Unmarshal(b)
	if err != nil {
		return err
	}
	u.toSketch()
	u.sk.Merge(s)
	return nil
}

// merge adds o's ids.
func (u *uniq) merge(o *uniq) {
	if o.sk != nil {
		u.toSketch()
		u.sk.Merge(o.sk)
		return
	}
	for h := range o.set {
		u.addHash(h)
	}
}

// count is the number of distinct ids: exact while no sketch joined, the sketch's estimate after.
func (u *uniq) count() int64 {
	if u.sk != nil {
		return u.sk.Count()
	}
	return u.n + int64(len(u.set))
}
