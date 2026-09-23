// Package webui embeds the worker's built-in static control panel.
//
// The panel is a small, self-contained SPA (index.html + app.css + app.js; no
// build step, no external assets) served from the worker's own port at "/", so
// it is SAME-ORIGIN with the Connect RPC: the browser needs only a bearer
// token, no CORS.
//
// It presents a login gate (bearer token, or one-time-code claim of an
// unclaimed worker), then a shell-style console (run commands with live
// streamed output, a client-side virtual cwd) with expandable Files and Jobs
// drawers. It talks to worker.v1.WorkerService + worker.v1.WorkerEnroll
// directly (including the server-streaming WatchJob).
package webui

import (
	"embed"
	"net/http"
	"strings"
)

//go:embed index.html app.css app.js
var assets embed.FS

// Handler serves the panel. Known assets are served by exact path; every other
// GET falls back to index.html (the app has no client-side routes, but this
// keeps deep links harmless).
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name, ctype := assetFor(r.URL.Path)
		data, err := assets.ReadFile(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(data)
	})
}

// assetFor maps a request path to an embedded file + content type. Unknown
// paths fall back to the SPA entry.
func assetFor(path string) (name, ctype string) {
	switch strings.TrimPrefix(path, "/") {
	case "app.css":
		return "app.css", "text/css; charset=utf-8"
	case "app.js":
		return "app.js", "text/javascript; charset=utf-8"
	default:
		return "index.html", "text/html; charset=utf-8"
	}
}
