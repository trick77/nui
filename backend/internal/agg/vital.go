package agg

import (
	"math"
	"slices"
)

// Web Vitals and engaged time as the aggregates hold them, for the builder and any raw read alike: a range over both
// adds up only while both take the same rows the same way. Go classifies, SQL only picks typed fields: no rounding of
// SQLite's beside Go's. Permanent once raw is gone (TestVitalsArePermanent): the order, the units, the version taken
// and the clamp; a change needs a builder version bump and must still read what older builds stored.

// VitalsVersion is the vitals' definition summed (the script's VITALS): Web Vitals' own, CLS the worst session window,
// INP the 98th percentile. A vital without it is from before (CLS the visit's sum, INP the slowest) and is left out.
const VitalsVersion = 2

// Vitals are the vitals summed, in the order their histograms are stored (agg_hour's v_lcp to v_ttfb).
var Vitals = [5]string{"LCP", "INP", "CLS", "FCP", "TTFB"}

// clsScale makes CLS, a score, whole units: the script's three decimals.
const clsScale = 1000

// MaxEngagedMS bounds one engagement's active milliseconds (EngagedSQL spells it): an hour. Old rows hold any value,
// a clock set back a negative one.
const MaxEngagedMS = 3_600_000

// The typed fields of a row as SQL over event: NULL (engaged 0) for another kind, for props that are not JSON (a row
// stored before /api/in checked them; json_type would fail the day's read on every pass) and for a field of another
// JSON type, so a string "2" or a 2.5 is never taken for 2. One WHEN each, kind first: CASE is SQLite's only
// guaranteed short circuit, other kinds' props are never parsed.
const (
	// VitalValueSQL is a vital's measured value, a number.
	VitalValueSQL = `CASE WHEN kind != 'vital' THEN NULL WHEN NOT json_valid(props) THEN NULL
		WHEN json_type(props, '$.p.value') IN ('integer', 'real') THEN json_extract(props, '$.p.value') END`
	// VitalVerSQL is a vital's definition version, an integer.
	VitalVerSQL = `CASE WHEN kind != 'vital' THEN NULL WHEN NOT json_valid(props) THEN NULL
		WHEN json_type(props, '$.p.v') = 'integer' THEN json_extract(props, '$.p.v') END`
	// EngagedSQL is an engagement's active milliseconds: a whole number clamped to 0..MaxEngagedMS, else 0. Always an
	// integer: past int64 json_extract gives a real, which the clamp replaces by its bound.
	EngagedSQL = `CASE WHEN kind != 'engagement' THEN 0 WHEN NOT json_valid(props) THEN 0
		WHEN json_type(props, '$.p.ms') = 'integer' THEN MIN(MAX(json_extract(props, '$.p.ms'), 0), 3600000) ELSE 0 END`
)

// VitalUnits classifies a vital by its name column and the values VitalVerSQL and VitalValueSQL scanned into any:
// its index in Vitals and its histogram units (ms; CLS ×1000), rounded, at most the histogram's last class. ok only
// for a known name, version exactly integer VitalsVersion and a finite value ≥ 0.
func VitalUnits(name string, ver, value any) (idx int, units int64, ok bool) {
	idx = slices.Index(Vitals[:], name)
	if v, isInt := ver.(int64); idx < 0 || !isInt || v != VitalsVersion {
		return 0, 0, false
	}
	var x float64
	switch v := value.(type) {
	case int64:
		x = float64(v)
	case float64:
		x = v
	default:
		return 0, 0, false
	}
	if !(x >= 0) || math.IsInf(x, 1) {
		return 0, 0, false
	}
	if name == "CLS" {
		x *= clsScale
	}
	return idx, int64(math.Round(min(x, histMax))), true
}

// vitalDims are the list dimensions of the vitals worth a page's list, by index in Vitals: one per vital, as an
// entry's sub is folded with max(). A page past a day's top TopK folds into its Other row, which keeps the sample
// count only: no histogram, so no percentile of a tail page.
var vitalDims = [len(Vitals)]string{"vital_lcp", "vital_inp", "vital_cls"}
