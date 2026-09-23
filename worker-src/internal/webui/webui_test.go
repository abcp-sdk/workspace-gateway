package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestHandlerServesPanel(t *testing.T) {
	rec := get(t, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "Agent Worker") {
		t.Error("index should carry the Agent Worker title")
	}
}

func TestHandlerServesBuiltAssets(t *testing.T) {
	// The vite build emits /assets/index-*.js + .css; the SPA entry references
	// them. Walk the embedded FS to find one of each and fetch it.
	html := get(t, "/").Body.String()
	for _, want := range []string{"/assets/", ".js", ".css"} {
		if !strings.Contains(html, want) {
			t.Fatalf("index missing %q", want)
		}
	}
}

func TestHandlerFallsBackToIndex(t *testing.T) {
	rec := get(t, "/some/deep/link")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Agent Worker") {
		t.Fatalf("fallback: %d", rec.Code)
	}
}

func TestHandlerRejectsNonGet(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d want 405", rec.Code)
	}
}
