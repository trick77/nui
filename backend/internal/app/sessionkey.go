package app

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/trick77/ship/kit/auth"
)

// SessionSecretFile is the session secret Stakeout keeps in the data directory when BACKEND_SESSION_SECRET is empty:
// made once, owner-only, reused across restarts.
const SessionSecretFile = "session.secret"

// sessionKey is the session cookie's signing key: HMAC-SHA256 keyed by the session secret (BACKEND_SESSION_SECRET,
// else the data directory's), over a label and the admin token. Never the token alone: a key derived from it alone
// lets anyone holding a signed cookie, even an expired one, test token guesses offline at hash speed. The token
// still goes in, so rotating it ends every session; changing the secret does too.
func sessionKey(cfg Config) ([]byte, error) {
	secret := []byte(cfg.SessionSecret)
	if len(secret) == 0 {
		s, err := loadSessionSecret(cfg.DataDir)
		if err != nil {
			return nil, err
		}
		secret = s
	}
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("stakeout-session\x00" + cfg.AdminToken))
	return m.Sum(nil), nil
}

// loadSessionSecret reads dir's session secret, making it on the first start. A directory Stakeout cannot write is a
// start-up error, never a key per start: that would sign everyone out on every restart, silently.
func loadSessionSecret(dir string) ([]byte, error) {
	path := filepath.Join(dir, SessionSecretFile)
	s, err := readSessionSecret(path)
	if !errors.Is(err, fs.ErrNotExist) {
		return s, err
	}
	s = []byte(hex.EncodeToString(auth.RandomKey()))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // a fixed name in the data dir
	if errors.Is(err, fs.ErrExist) {
		return readSessionSecret(path) // another process made it meanwhile
	}
	if err != nil {
		return nil, fmt.Errorf("creating the session secret: %w (make BACKEND_DATA_DIR writable or set "+
			"BACKEND_SESSION_SECRET)", err)
	}
	_, err = f.Write(append(s, '\n'))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("writing the session secret: %w", err)
	}
	slog.Info("Session secret created", "file", path)
	return s, nil
}

// readSessionSecret reads an existing secret, restricting a file others can read to its owner.
func readSessionSecret(path string) ([]byte, error) {
	b, err := os.ReadFile(path) //nolint:gosec // a fixed name in the data dir
	if errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("reading the session secret: %w", err)
	}
	s := bytes.TrimSpace(b)
	if len(s) < 32 {
		return nil, fmt.Errorf("%s holds %d bytes, want at least 32: delete it for a new one (signs everyone out)",
			path, len(s))
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("reading the session secret: %w", err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, fmt.Errorf("%s is readable by others and cannot be restricted: %w", path, err)
		}
		slog.Warn("Session secret was readable by others, restricted to its owner", "file", path)
	}
	return s, nil
}
