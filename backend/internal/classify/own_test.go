package classify

import (
	"strings"
	"testing"

	"github.com/trick77/trakk/backend/internal/refdata"
	"github.com/trick77/trakk/backend/internal/store"
)

const kumaUA = "Uptime-Kuma/2.5.3"

// The operator's own probe takes its address and its client together: another program from the address stays
// unknown, the same monitor software elsewhere is no claim on the probe, and a list's range over the address does not
// override it.
func TestPass_OwnProbe(t *testing.T) {
	r := newRig(t, true)
	own, err := refdata.ParseOwn([]byte(`{"services":{
	 "Home Kuma":{"ipv4":["198.51.100.50"],"user_agent_patterns":["Uptime-Kuma/*"]},
	 "Robot Kuma":{"ipv4":["69.162.124.231"],"user_agent_patterns":["Uptime-Kuma/*"]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	ref := refdata.New(refdata.Options{Dir: t.TempDir(), Own: own})
	b, err := refdata.ParseBots([]byte(crawlers), []byte(monitoring))
	if err != nil {
		t.Fatal(err)
	}
	ref.SetBots(b)
	r.c.Ref = ref
	r.insert(t,
		req("198.51.100.50", kumaUA, "", "/", 200, ""),
		req("198.51.100.50", kumaUA, "", "/", 200, ""),
		req("198.51.100.50", kumaUA, "", "/", 200, ""),
		// Another client from the own probe's address.
		req("198.51.100.50", "curl/8.4.0", "", "/", 200, ""),
		// The same software from elsewhere.
		req("198.51.100.51", kumaUA, "", "/", 200, ""),
		// An own probe inside UptimeRobot's authoritative range.
		req("69.162.124.231", kumaUA, "", "/", 200, ""),
	)
	// At the write, before any pass.
	cls := map[string]int{}
	rows, err := r.st.DB.Query(`SELECT ip, ua, cls FROM event`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var ip, ua string
		var c int
		if err := rows.Scan(&ip, &ua, &c); err != nil {
			t.Fatal(err)
		}
		cls[ip+" "+ua] = c
	}
	_ = rows.Close()
	if c := cls["198.51.100.50 "+kumaUA]; c != store.ClsMonitor {
		t.Errorf("own probe written as %d", c)
	}
	if c := cls["198.51.100.51 "+kumaUA]; c != store.ClsUnknown {
		t.Errorf("Kuma elsewhere written as %d", c)
	}
	if err := r.c.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	classes, err := r.st.ActorClasses(t.Context(), r.site.ID, r.now.UnixMilli()/dayMS)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"198.51.100.50":  "monitor|own probe Home Kuma,client curl/8.4.0",
		"198.51.100.51":  "unknown|client Uptime-Kuma/2.5.3",
		"69.162.124.231": "monitor|own probe Robot Kuma",
	} {
		a := classes[key]
		if got := a.Class + "|" + strings.Join(a.Reasons, ","); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	// The curl beside the probe keeps its own class, the probe's events carry its name.
	var curl int
	var svc string
	_ = r.st.DB.QueryRow(`SELECT cls FROM event WHERE ip = '198.51.100.50' AND ua = 'curl/8.4.0'`).Scan(&curl)
	if curl != store.ClsUnknown {
		t.Errorf("curl beside the own probe: %d", curl)
	}
	_ = r.st.DB.QueryRow(`SELECT svc FROM event WHERE ip = '198.51.100.50' AND ua = ?`, kumaUA).Scan(&svc)
	if svc != "Home Kuma" {
		t.Errorf("own probe named %q", svc)
	}
	_ = r.st.DB.QueryRow(`SELECT svc FROM event WHERE ip = '69.162.124.231'`).Scan(&svc)
	if svc != "Robot Kuma" {
		t.Errorf("own probe in a list's range named %q", svc)
	}
	if n, ok := ref.Bots().ByName("Home Kuma"); !ok || n.Class != "monitor" {
		t.Errorf("ByName = %+v, %v", n, ok)
	}
}
