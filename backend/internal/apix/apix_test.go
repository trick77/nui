package apix

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWriters(t *testing.T) {
	w := httptest.NewRecorder()
	Error(w, http.StatusForbidden, "forbidden")
	if w.Code != 403 || w.Body.String() != "{\"detail\":\"forbidden\"}\n" {
		t.Errorf("Error = %d %q", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	TooMany(w, 1500*time.Millisecond)
	if w.Code != 429 || w.Header().Get("Retry-After") != "2" {
		t.Errorf("TooMany = %d %v", w.Code, w.Header())
	}
	w = httptest.NewRecorder()
	JSON(w, 200, map[string]int{"a": 1})
	if w.Body.String() != "{\"a\":1}\n" || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("JSON = %q", w.Body)
	}
	w = httptest.NewRecorder()
	ServerError(w, httptest.NewRequest("GET", "/", nil), errors.New("boom"))
	if w.Code != 500 || w.Body.String() != "{\"detail\":\"internal server error\"}\n" {
		t.Errorf("ServerError = %d %q", w.Code, w.Body)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w = httptest.NewRecorder()
	ServerError(w, httptest.NewRequest("GET", "/", nil).WithContext(ctx), errors.New("gone"))
	if w.Code != 500 {
		t.Errorf("cancelled = %d", w.Code)
	}
}
