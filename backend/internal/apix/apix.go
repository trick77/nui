// Package apix writes nui's JSON answers. Errors are {"detail": "…"} (the API contract), where kit's httpx writes
// {"error": "…"}; everything else is kit's.
package apix

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/trick77/ship/kit/httpx"
	"github.com/trick77/ship/kit/logging"
)

// Sep separates two pieces of text the dashboard shows verbatim: a middle dot with an en space each side, plain
// spaces read cramped in the family's fonts. The UI's is SEP in format.ts.
const Sep = ensp + "·" + ensp

// ensp is U+2002 by its code point: the raw character is invisible in review.
const ensp = string(rune(0x2002))

// plainSep is a middle dot between plain spaces: the spelling before Sep, which stored reasons of past days hold and
// the model writes.
const plainSep = " · "

// Respace spells every plainSep in s as Sep.
func Respace(s string) string { return strings.ReplaceAll(s, plainSep, Sep) }

// JSON writes v with status, uncached.
func JSON(w http.ResponseWriter, status int, v any) { httpx.WriteJSON(w, status, v) }

// Error writes {"detail": msg}. msg reaches the client: a fixed phrase, never an internal error.
func Error(w http.ResponseWriter, status int, msg string) {
	httpx.WriteJSON(w, status, map[string]string{"detail": msg})
}

// TooMany writes a 429 with Retry-After.
func TooMany(w http.ResponseWriter, retryAfter time.Duration) {
	httpx.SetRetryAfter(w, retryAfter)
	Error(w, http.StatusTooManyRequests, "rate limited")
}

// ServerError logs err and answers 500 with a fixed detail; a request the client gave up on logs at warn.
func ServerError(w http.ResponseWriter, r *http.Request, err error) {
	ctx := r.Context()
	if errors.Is(ctx.Err(), context.Canceled) {
		slog.WarnContext(ctx, "Request cancelled", logging.Err(err))
	} else {
		slog.ErrorContext(ctx, "Request failed", logging.Err(err))
	}
	Error(w, http.StatusInternalServerError, "internal server error")
}
