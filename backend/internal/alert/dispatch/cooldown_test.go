package dispatch

import (
	"testing"
	"time"
)

// A fact missing from one hourly pass and back at the next stays quiet with the default cooldown.
func TestDefaultCooldownSpansAPass(t *testing.T) {
	r := newRig(t, `{"channels": {"ops": "ntfy://ntfy.example.com/t"}, "defaults": {"channels": ["ops"]},
		"rules": [{"name": "errs", "on": "error.new"}]}`)
	r.step(0)
	r.src.occ["errs"] = occ("a")
	r.step(time.Hour)
	r.src.occ["errs"] = nil
	r.step(time.Hour)
	r.src.occ["errs"] = occ("a")
	r.step(time.Hour)
	if msgs := r.rec.Messages(); len(msgs) != 1 {
		t.Fatalf("messages %+v", msgs)
	}
}
