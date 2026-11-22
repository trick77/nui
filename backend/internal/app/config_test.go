package app

import "testing"

func lookup(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestLoadConfig_Defaults(t *testing.T) {
	cfg, err := LoadConfig(lookup(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Addr: ":8080", DataDir: "/data", LogLevel: "info"}
	if cfg != want {
		t.Errorf("cfg = %+v, want %+v", cfg, want)
	}
}

func TestLoadConfig_FromEnv(t *testing.T) {
	cfg, err := LoadConfig(lookup(map[string]string{
		"BACKEND_ADDR":      "127.0.0.1:9090",
		"BACKEND_DATA_DIR":  "/var/lib/nui",
		"BACKEND_LOG_LEVEL": "debug",
		"PUBLIC_FQDN":       "nui.example.com",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Addr: "127.0.0.1:9090", DataDir: "/var/lib/nui", PublicFQDN: "nui.example.com", LogLevel: "debug"}
	if cfg != want {
		t.Errorf("cfg = %+v, want %+v", cfg, want)
	}
}
