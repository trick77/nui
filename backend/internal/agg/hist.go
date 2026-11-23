package agg

import (
	"encoding/binary"
	"errors"
	"slices"
	"sort"
)

// The duration histogram: counts per class of milliseconds, added up over days and read for percentiles. Its classes
// and encoding are permanent once raw events are purged, as the sketch's are: a change needs a new version byte and
// must still read the old one. The raw read fills the same classes, so a range over both answers alike.

// histMax is the last class's start: an hour. Longer counts there.
const histMax = 3_600_000

// histStarts are the classes' lowest values: every millisecond to 23, then 9 % apart.
var histStarts = func() []int64 {
	out := []int64{0}
	for v := int64(1); v < histMax; v += max(1, v*9/100) {
		out = append(out, v)
	}
	return append(out, histMax)
}()

const histV1 = 1

var errHist = errors.New("agg: malformed histogram")

// Hist is a histogram; the zero value is empty. It holds the classes counted, in order: a key of few values stays
// small (a sweep makes a key per path).
type Hist struct {
	classes []histClass
	n       int64
}

type histClass struct {
	class int
	n     int64
}

// Add counts ms n times.
func (h *Hist) Add(ms, n int64) {
	if n <= 0 {
		return
	}
	h.add(sort.Search(len(histStarts), func(i int) bool { return histStarts[i] > max(ms, 0) })-1, n)
}

func (h *Hist) add(class int, n int64) {
	i := sort.Search(len(h.classes), func(i int) bool { return h.classes[i].class >= class })
	if i == len(h.classes) || h.classes[i].class != class {
		h.classes = slices.Insert(h.classes, i, histClass{class: class})
	}
	h.classes[i].n += n
	h.n += n
}

// Merge adds o's counts.
func (h *Hist) Merge(o *Hist) {
	for _, c := range o.classes {
		h.add(c.class, c.n)
	}
}

// Count is how many values it holds.
func (h *Hist) Count() int64 { return h.n }

// Percentile is the p-th percentile (0 < p <= 100) in ms by nearest rank, as its class's middle: within 5 % of the
// exact value below an hour. ok is false for an empty histogram.
func (h *Hist) Percentile(p float64) (ms int64, ok bool) {
	if h.n == 0 {
		return 0, false
	}
	rank := max(1, int64(float64(h.n)*p/100+0.999999))
	for _, c := range h.classes {
		if rank -= c.n; rank <= 0 {
			if c.class == len(histStarts)-1 {
				return histMax, true
			}
			return (histStarts[c.class] + histStarts[c.class+1] - 1) / 2, true
		}
	}
	return histMax, true
}

// Marshal encodes it: a version byte, then per counted class its distance from the one before and its count, as
// uvarints. nil when empty.
func (h *Hist) Marshal() []byte {
	if h.n == 0 {
		return nil
	}
	out := []byte{histV1}
	last := 0
	for _, c := range h.classes {
		out = binary.AppendUvarint(out, uint64(c.class-last)) //nolint:gosec // G115: the classes ascend
		out = binary.AppendUvarint(out, uint64(c.n))          //nolint:gosec // G115: a count
		last = c.class
	}
	return out
}

// AddEncoded adds an encoded histogram's counts; nil adds nothing. A malformed one leaves h unchanged.
func (h *Hist) AddEncoded(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	if b[0] != histV1 {
		return errHist
	}
	var add Hist
	b = b[1:]
	i := uint64(0)
	for len(b) > 0 {
		d, n := binary.Uvarint(b)
		if n <= 0 {
			return errHist
		}
		c, m := binary.Uvarint(b[n:])
		if m <= 0 || d >= uint64(len(histStarts)) || i+d >= uint64(len(histStarts)) || c == 0 || c > 1<<62 {
			return errHist
		}
		i += d
		add.add(int(i), int64(c)) //nolint:gosec // G115: both checked above
		b = b[n+m:]
	}
	h.Merge(&add)
	return nil
}
