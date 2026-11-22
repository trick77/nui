package app

import (
	"path/filepath"
	"strings"

	"github.com/trick77/ship/kit/env"
	"github.com/trick77/ship/kit/logging"
)

// Config is the runtime configuration: BACKEND_* variables, plus PUBLIC_FQDN, which the Traefik rule reads too (one
// name, so the app and the router never disagree on where it lives). The model's key is llmwire's own variable
// (LLMWIRE_MIMO_API_KEY), read by llmwire.FromEnv.
type Config struct {
	// Addr is the listen address; the -healthcheck probe dials it too.
	Addr string
	// DataDir is the container's volume: the database, the reference data cache, the sites file.
	DataDir string
	// PublicFQDN is the hostname the service is reached on; empty for local use.
	PublicFQDN string
	// LogLevel is debug, info, warn or error.
	LogLevel string

	// SitesFile declares the sites, their feed key hashes and path rules (docs/sites.example.json).
	SitesFile string
	// RetentionMonths is how long raw events are kept: 25, so a 12-month view still has its comparison year.
	RetentionMonths int
	// MaxDBMB caps what the rows may take; past it the oldest events go first. 0 is no cap.
	MaxDBMB int

	// SessionSecret signs the session cookie, 32+ characters; empty signs with a random key per start.
	SessionSecret string
	OIDCIssuer, OIDCClientID, OIDCClientSecret, OIDCRedirectURL, OIDCAdminGroup string
	// AuthDisabled serves the dashboard without sign-in, for local development only: refused with PUBLIC_FQDN set.
	AuthDisabled bool

	// RefDataDisabled skips downloading ipverse's reference data (offline development).
	RefDataDisabled bool

	// LLMModel is the llmwire profile the background writer uses.
	LLMModel string
	// LLMDailyCalls and LLMDailyTokens are each site's daily model budget.
	LLMDailyCalls, LLMDailyTokens int
}

// LoadConfig reads the configuration through lookup, every fault at once.
func LoadConfig(lookup func(string) string) (Config, error) {
	r := env.New(lookup)
	cfg := Config{
		Addr:             r.String("BACKEND_ADDR", ":8080"),
		DataDir:          r.String("BACKEND_DATA_DIR", "/data"),
		PublicFQDN:       r.String("PUBLIC_FQDN", ""),
		LogLevel:         r.String("BACKEND_LOG_LEVEL", "info"),
		SitesFile:        r.String("BACKEND_SITES_FILE", ""),
		RetentionMonths:  r.Int("BACKEND_RETENTION_MONTHS", 25),
		MaxDBMB:          r.Int("BACKEND_MAX_DB_MB", 20480),
		SessionSecret:    r.String("BACKEND_SESSION_SECRET", ""),
		OIDCIssuer:       r.String("BACKEND_OIDC_ISSUER", ""),
		OIDCClientID:     r.String("BACKEND_OIDC_CLIENT_ID", ""),
		OIDCClientSecret: r.String("BACKEND_OIDC_CLIENT_SECRET", ""),
		OIDCRedirectURL:  r.String("BACKEND_OIDC_REDIRECT_URL", ""),
		OIDCAdminGroup:   r.String("BACKEND_OIDC_ADMIN_GROUP", ""),
		AuthDisabled:     r.Bool("BACKEND_AUTH_DISABLED", false),
		RefDataDisabled:  r.Bool("BACKEND_REFDATA_DISABLED", false),
		LLMModel:         r.String("BACKEND_LLM_MODEL", "mimo-v2.6-flash"),
		LLMDailyCalls:    r.Int("BACKEND_LLM_DAILY_CALLS", 200),
		LLMDailyTokens:   r.Int("BACKEND_LLM_DAILY_TOKENS", 400_000),
	}
	if cfg.SitesFile == "" {
		cfg.SitesFile = filepath.Join(cfg.DataDir, "sites.json")
	}
	if cfg.OIDCRedirectURL == "" && cfg.PublicFQDN != "" {
		cfg.OIDCRedirectURL = "https://" + cfg.PublicFQDN + "/api/auth/callback"
	}
	if _, err := logging.ParseLevel(cfg.LogLevel); err != nil {
		r.Fail("BACKEND_LOG_LEVEL: %v", err)
	}
	if cfg.RetentionMonths < 1 {
		r.Fail("BACKEND_RETENTION_MONTHS must be at least 1")
	}
	if cfg.MaxDBMB < 0 || cfg.LLMDailyCalls < 0 || cfg.LLMDailyTokens < 0 {
		r.Fail("BACKEND_MAX_DB_MB, BACKEND_LLM_DAILY_CALLS and BACKEND_LLM_DAILY_TOKENS cannot be negative")
	}
	if cfg.SessionSecret != "" && len(cfg.SessionSecret) < 32 {
		r.Fail("BACKEND_SESSION_SECRET is shorter than 32 characters")
	}
	// OIDC is all or nothing: half a configuration would fail at the first sign-in instead of at start.
	if cfg.OIDCIssuer != "" || cfg.OIDCClientID != "" || cfg.OIDCClientSecret != "" {
		for _, f := range []struct{ name, v string }{
			{"BACKEND_OIDC_ISSUER", cfg.OIDCIssuer}, {"BACKEND_OIDC_CLIENT_ID", cfg.OIDCClientID},
			{"BACKEND_OIDC_CLIENT_SECRET", cfg.OIDCClientSecret}, {"BACKEND_OIDC_REDIRECT_URL", cfg.OIDCRedirectURL},
		} {
			if f.v == "" {
				r.Fail("%s is not set (OIDC needs issuer, client id, client secret and redirect URL)", f.name)
			}
		}
	}
	if cfg.AuthDisabled && cfg.PublicFQDN != "" {
		r.Fail("BACKEND_AUTH_DISABLED is for local development: it is refused with PUBLIC_FQDN set")
	}
	return cfg, r.Err()
}

// OIDCConfigured reports whether sign-in is configured.
func (c Config) OIDCConfigured() bool { return c.OIDCIssuer != "" }

// Secure reports whether cookies get the Secure flag: the service is reached over https (Traefik terminates TLS, so
// nothing in the request says so).
func (c Config) Secure() bool {
	return c.PublicFQDN != "" || strings.HasPrefix(c.OIDCRedirectURL, "https://")
}
