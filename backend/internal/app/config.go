package app

import (
	"errors"
	"io/fs"
	"os"
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
	// DataDir is the container's writable volume: the database, the reference data cache, the session secret.
	DataDir string
	// ConfDir is the operator's configuration, mounted read-only: the sites file.
	ConfDir string
	// PublicFQDN is the hostname the service is reached on; empty for local use.
	PublicFQDN string
	// TrustedHops is the number of proxies in front of the server, each appending to X-Forwarded-For: 1 behind the
	// compose file's Traefik, 0 reached directly (the connection's peer is the caller). Every client address (the
	// collector's, the sign-in throttle's, the access log's) is read through it (apix.ClientAddr).
	TrustedHops int
	// LogLevel is debug, info, warn or error.
	LogLevel string

	// SitesFile declares the sites, their feed key hashes and path rules (docs/sites.example.json).
	SitesFile string
	// sitesDefault: SitesFile is the default in ConfDir, not BACKEND_SITES_FILE (SitesPath may fall back).
	sitesDefault bool
	// NotifyFile holds the alert rules and the channels they send to (docs/notify.example.json). Read at start: a
	// file with faults never stops it, it sends nothing then.
	NotifyFile string
	// RetentionMonths is how long the per-address aggregates, actor classes and model usage are kept, and raw events
	// at most; the other figures stay, unless the size cap takes them.
	RetentionMonths int
	// RawRetentionDays is how long raw events are kept (store.PurgeRaw): at least MinRawRetentionDays, at most
	// RetentionMonths in effect.
	RawRetentionDays int
	// MaxDBMB caps what the rows may take; past it the oldest raw events go first, then aggregates
	// (store.EnforceCap). 0 is no cap.
	MaxDBMB int

	// SessionSecret keys the session cookie's signature (with the admin token, sessionKey), 32+ characters; empty
	// uses a secret Trakk makes once in DataDir (SessionSecretFile).
	SessionSecret string

	// OIDC: all four of issuer, client id, secret and redirect URL, or none; the group is optional.
	OIDCIssuer, OIDCClientID, OIDCClientSecret, OIDCRedirectURL, OIDCAdminGroup string
	// AdminToken signs in with a token from the environment: beside OIDC the way in when the provider is down,
	// without it the only one. Empty turns token sign-in off.
	AdminToken string
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
		ConfDir:          r.String("BACKEND_CONF_DIR", "/conf"),
		PublicFQDN:       r.String("PUBLIC_FQDN", ""),
		TrustedHops:      r.Int("BACKEND_TRUSTED_HOPS", 1),
		LogLevel:         r.String("BACKEND_LOG_LEVEL", "info"),
		SitesFile:        r.String("BACKEND_SITES_FILE", ""),
		NotifyFile:       r.String("BACKEND_NOTIFY_FILE", ""),
		RetentionMonths:  r.Int("BACKEND_RETENTION_MONTHS", 25),
		RawRetentionDays: r.Int("BACKEND_RAW_RETENTION_DAYS", 90),
		MaxDBMB:          r.Int("BACKEND_MAX_DB_MB", 20480),
		SessionSecret:    r.String("BACKEND_SESSION_SECRET", ""),
		OIDCIssuer:       r.String("BACKEND_OIDC_ISSUER", ""),
		OIDCClientID:     r.String("BACKEND_OIDC_CLIENT_ID", ""),
		OIDCClientSecret: r.String("BACKEND_OIDC_CLIENT_SECRET", ""),
		OIDCRedirectURL:  r.String("BACKEND_OIDC_REDIRECT_URL", ""),
		OIDCAdminGroup:   r.String("BACKEND_OIDC_ADMIN_GROUP", ""),
		AdminToken:       r.String("BACKEND_ADMIN_TOKEN", ""),
		AuthDisabled:     r.Bool("BACKEND_AUTH_DISABLED", false),
		RefDataDisabled:  r.Bool("BACKEND_REFDATA_DISABLED", false),
		LLMModel:         r.String("BACKEND_LLM_MODEL", "mimo-v2.6-flash"),
		LLMDailyCalls:    r.Int("BACKEND_LLM_DAILY_CALLS", 200),
		LLMDailyTokens:   r.Int("BACKEND_LLM_DAILY_TOKENS", 400_000),
	}
	if cfg.SitesFile == "" {
		cfg.SitesFile, cfg.sitesDefault = filepath.Join(cfg.ConfDir, SitesFileName), true
	}
	if cfg.NotifyFile == "" {
		cfg.NotifyFile = filepath.Join(cfg.ConfDir, "notify.json")
	}
	if cfg.OIDCRedirectURL == "" && cfg.PublicFQDN != "" {
		cfg.OIDCRedirectURL = "https://" + cfg.PublicFQDN + "/api/auth/callback"
	}
	if _, err := logging.ParseLevel(cfg.LogLevel); err != nil {
		r.Fail("BACKEND_LOG_LEVEL: %v", err)
	}
	if cfg.TrustedHops < 0 || cfg.TrustedHops > 8 {
		r.Fail("BACKEND_TRUSTED_HOPS must be 0 (no proxy) to 8")
	}
	if cfg.RetentionMonths < 1 {
		r.Fail("BACKEND_RETENTION_MONTHS must be at least 1")
	}
	if cfg.RawRetentionDays < MinRawRetentionDays {
		r.Fail("BACKEND_RAW_RETENTION_DAYS must be at least %d: 7d and its prior period read raw two weeks back",
			MinRawRetentionDays)
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
	switch n := len(cfg.AdminToken); {
	case n > 0 && n < MinAdminTokenBytes:
		r.Fail("BACKEND_ADMIN_TOKEN is shorter than %d bytes (openssl rand -hex 32)", MinAdminTokenBytes)
	case n > MaxAdminTokenBytes:
		// Longer would start a service whose sign-in answers every attempt 413.
		r.Fail("BACKEND_ADMIN_TOKEN is longer than %d bytes", MaxAdminTokenBytes)
	}
	if cfg.AuthDisabled && cfg.PublicFQDN != "" {
		r.Fail("BACKEND_AUTH_DISABLED is for local development: it is refused with PUBLIC_FQDN set")
	}
	if cfg.PublicFQDN != "" && !cfg.AuthDisabled && !cfg.OIDCConfigured() && cfg.AdminToken == "" {
		r.Fail("no sign-in is configured: set the BACKEND_OIDC_* variables, BACKEND_ADMIN_TOKEN or both")
	}
	return cfg, r.Err()
}

// SitesFileName is the sites file's name in the conf directory (and, before it, in the data directory).
const SitesFileName = "sites.json"

// SitesPath is the sites file to read. The default in the conf directory falls back to the data directory's, where
// Trakk kept it before, while the conf one is missing and the data one exists (legacy true): without the fallback an
// updated host with the old layout would refuse every event. BACKEND_SITES_FILE set is read as it is.
func (c Config) SitesPath() (path string, legacy bool) {
	if !c.sitesDefault {
		return c.SitesFile, false
	}
	if _, err := os.Stat(c.SitesFile); !errors.Is(err, fs.ErrNotExist) {
		return c.SitesFile, false
	}
	old := filepath.Join(c.DataDir, SitesFileName)
	if _, err := os.Stat(old); err == nil {
		return old, true
	}
	return c.SitesFile, false
}

// MinRawRetentionDays is the shortest raw retention. 7d and its prior period read raw back to the local midnight 13
// days before today's: a piece off the aggregates' grain (a zone on a half hour, a local midnight inside a UTC day)
// has nothing else to be read from, and at any UTC offset (-12 to +14 h) that midnight falls on a UTC day at most 14
// days back. One day more as a margin; far above the days not built yet (store.CapTail).
const MinRawRetentionDays = 15

// Bounds of BACKEND_ADMIN_TOKEN. The minimum refuses a guessable token (`openssl rand -hex 32` makes 64); the
// maximum keeps the longest token inside the sign-in form's body cap, every byte url-encoded as %XX.
const (
	MinAdminTokenBytes = 32
	MaxAdminTokenBytes = 2 << 10
)

// TokenConfigured reports whether token sign-in is on.
func (c Config) TokenConfigured() bool { return c.AdminToken != "" }

// OIDCConfigured reports whether sign-in is configured.
func (c Config) OIDCConfigured() bool { return c.OIDCIssuer != "" }

// Secure reports whether cookies get the Secure flag: the service is reached over https (Traefik terminates TLS, so
// nothing in the request says so).
func (c Config) Secure() bool {
	return c.PublicFQDN != "" || strings.HasPrefix(c.OIDCRedirectURL, "https://")
}
