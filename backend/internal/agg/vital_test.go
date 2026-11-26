package agg

import (
	"math"
	"strings"
	"testing"
)

// The vitals' histograms are stored for good: their order, CLS's factor, the version taken and the engaged clamp are
// pinned.
func TestVitalsArePermanent(t *testing.T) {
	if Vitals != [5]string{"LCP", "INP", "CLS", "FCP", "TTFB"} {
		t.Errorf("order %v", Vitals)
	}
	if VitalsVersion != 2 || clsScale != 1000 || MaxEngagedMS != 3_600_000 {
		t.Errorf("version %d, CLS ×%d, engaged up to %d", VitalsVersion, clsScale, MaxEngagedMS)
	}
	if !strings.Contains(EngagedSQL, "MIN(MAX(json_extract(props, '$.p.ms'), 0), 3600000)") {
		t.Errorf("EngagedSQL clamps otherwise: %s", EngagedSQL)
	}
	if vitalDims != [5]string{"vital_lcp", "vital_inp", "vital_cls"} {
		t.Errorf("vital dimensions %v", vitalDims)
	}
}

// Only a known vital of version integer 2 with a finite value ≥ 0 counts, whatever the scan made of the rest.
func TestVitalUnits(t *testing.T) {
	for _, tt := range []struct {
		name       string
		ver, value any
		idx        int
		units      int64
		ok         bool
	}{
		{"LCP", int64(2), 1234.5, 0, 1235, true},
		{"LCP", int64(2), int64(1200), 0, 1200, true},
		{"INP", int64(2), 0.4, 1, 0, true},
		{"CLS", int64(2), 0.0425, 2, int64(math.Round(0.0425 * 1000)), true},
		{"CLS", int64(2), 0.1, 2, 100, true},
		{"FCP", int64(2), 900.0, 3, 900, true},
		{"TTFB", int64(2), 1e300, 4, 3_600_000, true},
		{"CLS", int64(2), 1e300, 2, 3_600_000, true},
		{"LCP", nil, 1200.0, 0, 0, false},           // the old script's
		{"LCP", "2", 1200.0, 0, 0, false},           // a string
		{"LCP", 2.0, 1200.0, 0, 0, false},           // a real
		{"LCP", 2.5, 1200.0, 0, 0, false},           // a fraction
		{"LCP", int64(1), 1200.0, 0, 0, false},      // another definition
		{"LCP", int(2), 1200.0, 0, 0, false},        // not as scanned
		{"LCP", int64(2), "1200", 0, 0, false},      // a string value
		{"LCP", int64(2), nil, 0, 0, false},         // none
		{"INP", int64(2), -5.0, 0, 0, false},        // negative
		{"INP", int64(2), int64(-5), 0, 0, false},   // negative
		{"INP", int64(2), math.NaN(), 0, 0, false},  // no number
		{"INP", int64(2), math.Inf(1), 0, 0, false}, // unbounded
		{"FOO", int64(2), 10.0, 0, 0, false},        // no vital
		{"lcp", int64(2), 10.0, 0, 0, false},        // another name
	} {
		idx, units, ok := VitalUnits(tt.name, tt.ver, tt.value)
		if idx != tt.idx || units != tt.units || ok != tt.ok {
			t.Errorf("%s %#v %#v = %d %d %v, want %d %d %v", tt.name, tt.ver, tt.value, idx, units, ok, tt.idx,
				tt.units, tt.ok)
		}
	}
}
