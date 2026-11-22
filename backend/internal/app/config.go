package app

import "github.com/trick77/ship/kit/env"

// Config is the runtime configuration: BACKEND_* variables, plus PUBLIC_FQDN, which the Traefik rule reads too (one
// name, so the app and the router never disagree on where it lives).
type Config struct {
	// Addr is the listen address; the -healthcheck probe dials it too.
	Addr string
	// DataDir is the container's volume.
	DataDir string
	// PublicFQDN is the hostname the service is reached on; empty for local use.
	PublicFQDN string
	// LogLevel is debug, info, warn or error; kit/logging parses and rejects it.
	LogLevel string
}

// LoadConfig reads the configuration through lookup, every fault at once.
func LoadConfig(lookup func(string) string) (Config, error) {
	r := env.New(lookup)
	cfg := Config{
		Addr:       r.String("BACKEND_ADDR", ":8080"),
		DataDir:    r.String("BACKEND_DATA_DIR", "/data"),
		PublicFQDN: r.String("PUBLIC_FQDN", ""),
		LogLevel:   r.String("BACKEND_LOG_LEVEL", "info"),
	}
	return cfg, r.Err()
}
