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
	body := rec.Body.String()
	for _, want := range []string{"Agent Worker", "id=\"gate\"", "id=\"term\"", "/app.js", "/app.css"} {
		if !strings.Contains(body, want) {
			t.Errorf("index missing %q", want)
		}
	}
}

func TestHandlerServesAssets(t *testing.T) {
	css := get(t, "/app.css")
	if css.Code != http.StatusOK || !strings.HasPrefix(css.Header().Get("Content-Type"), "text/css") {
		t.Fatalf("css: %d %q", css.Code, css.Header().Get("Content-Type"))
	}
	js := get(t, "/app.js")
	if js.Code != http.StatusOK || !strings.HasPrefix(js.Header().Get("Content-Type"), "text/javascript") {
		t.Fatalf("js: %d %q", js.Code, js.Header().Get("Content-Type"))
	}
	if !strings.Contains(js.Body.String(), "/worker.v1.WorkerService/") {
		t.Error("app.js should reference the WorkerService RPC base")
	}
	if !strings.Contains(js.Body.String(), "/worker.v1.WorkerEnroll/") {
		t.Error("app.js should reference the WorkerEnroll RPC base")
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
