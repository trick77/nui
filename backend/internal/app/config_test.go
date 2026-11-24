package app

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func lookup(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestLoadConfig_Defaults(t *testing.T) {
	cfg, err := LoadConfig(lookup(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TrustedHops != 1 {
		t.Errorf("trusted hops default %d, want 1 (the compose file's Traefik)", cfg.TrustedHops)
	}
	if cfg.Addr != ":8080" || cfg.DataDir != "/data" || cfg.SitesFile != "/data/sites.json" || cfg.RetentionMonths != 25 ||
		cfg.RawRetentionDays != 90 || cfg.MaxDBMB != 20480 || cfg.LLMModel != "mimo-v2.6-flash" || cfg.LLMDailyCalls != 200 || cfg.OIDCConfigured() ||
		cfg.Secure() || cfg.AuthDisabled {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestLoadConfig_FromEnv(t *testing.T) {
	cfg, err := LoadConfig(lookup(map[string]string{
		"BACKEND_ADDR":               "127.0.0.1:9090",
		"BACKEND_DATA_DIR":           "/var/lib/trakk",
		"BACKEND_LOG_LEVEL":          "debug",
		"PUBLIC_FQDN":                "trakk.example.com",
		"BACKEND_OIDC_ISSUER":        "https://auth.example.com",
		"BACKEND_OIDC_CLIENT_ID":     "trakk",
		"BACKEND_OIDC_CLIENT_SECRET": "s",
		"BACKEND_OIDC_ADMIN_GROUP":   "trakk-admins",
		"BACKEND_SESSION_SECRET":     strings.Repeat("x", 32),
		"BACKEND_RETENTION_MONTHS":   "13",
		"BACKEND_RAW_RETENTION_DAYS": "15",
		"BACKEND_TRUSTED_HOPS":       "0",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OIDCRedirectURL != "https://trakk.example.com/api/auth/callback" || !cfg.Secure() || !cfg.OIDCConfigured() ||
		cfg.SitesFile != "/var/lib/trakk/sites.json" || cfg.RetentionMonths != 13 || cfg.RawRetentionDays != 15 ||
		cfg.TrustedHops != 0 {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestLoadConfig_Faults(t *testing.T) {
	for name, vars := range map[string]map[string]string{
		"level":       {"BACKEND_LOG_LEVEL": "loud"},
		"retention":   {"BACKEND_RETENTION_MONTHS": "0"},
		"raw days":    {"BACKEND_RAW_RETENTION_DAYS": "14"},
		"raw days 0":  {"BACKEND_RAW_RETENTION_DAYS": "0"},
		"raw words":   {"BACKEND_RAW_RETENTION_DAYS": "ninety"},
		"negative":    {"BACKEND_MAX_DB_MB": "-1"},
		"secret":      {"BACKEND_SESSION_SECRET": "short"},
		"half oidc":   {"BACKEND_OIDC_ISSUER": "https://auth.example.com"},
		"auth off":    {"BACKEND_AUTH_DISABLED": "true", "PUBLIC_FQDN": "trakk.example.com"},
		"not a bool":  {"BACKEND_AUTH_DISABLED": "maybe"},
		"not numeric": {"BACKEND_LLM_DAILY_CALLS": "many"},
		"no sign-in":  {"PUBLIC_FQDN": "trakk.example.com"},
		"short token": {"BACKEND_ADMIN_TOKEN": strings.Repeat("t", MinAdminTokenBytes-1)},
		"long token":  {"BACKEND_ADMIN_TOKEN": strings.Repeat("t", MaxAdminTokenBytes+1)},
		"hops":        {"BACKEND_TRUSTED_HOPS": "-1"},
		"many hops":   {"BACKEND_TRUSTED_HOPS": "9"},
	} {
		if _, err := LoadConfig(lookup(vars)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	_, err := LoadConfig(lookup(map[string]string{"PUBLIC_FQDN": "trakk.example.com"}))
	if err == nil || !strings.Contains(err.Error(), "no sign-in is configured") {
		t.Errorf("production without sign-in: %v", err)
	}
	// The admin token alone is a sign-in.
	if cfg, err := LoadConfig(lookup(map[string]string{"PUBLIC_FQDN": "trakk.example.com",
		"BACKEND_ADMIN_TOKEN": strings.Repeat("t", MinAdminTokenBytes)})); err != nil || !cfg.TokenConfigured() {
		t.Errorf("production with a token: %v", err)
	}
	if cfg, err := LoadConfig(lookup(map[string]string{"BACKEND_AUTH_DISABLED": "true"})); err != nil || !cfg.AuthDisabled {
		t.Errorf("local auth off: %v", err)
	}
}

// TestEnvExample keeps .env.example complete: every variable the config reads, and llmwire's key, has a line.
func TestEnvExample(t *testing.T) {
	read := map[string]bool{}
	_, _ = LoadConfig(func(k string) string { read[k] = true; return "" })
	b, err := os.ReadFile(filepath.Join("..", "..", "..", ".env.example"))
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^#?\s*([A-Z][A-Z0-9_]+)=`).FindAllStringSubmatch(string(b), -1) {
		listed[m[1]] = true
	}
	var missing []string
	for k := range read {
		if !listed[k] {
			missing = append(missing, k)
		}
	}
	for _, k := range []string{"LLMWIRE_MIMO_API_KEY"} {
		if !listed[k] {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf(".env.example lacks %v", missing)
	}
}
