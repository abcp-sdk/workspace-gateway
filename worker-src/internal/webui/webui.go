// Package webui embeds the worker's built-in control panel.
//
// The panel is a Svelte 5 + Tailwind 4 SPA (built by webui/ via vite into
// dist/, embedded here) served from the worker's own port at "/", so it is
// SAME-ORIGIN with the Connect RPC: the browser needs only a bearer token, no
// CORS.
//
// It presents a login gate (bearer token, or one-time-code claim of an
// unclaimed worker), then a shell-style console (run commands with live
// streamed output, a client-side virtual cwd) with mutually-exclusive Files
// and Jobs drawers. It talks to worker.v1.WorkerService + worker.v1.WorkerEnroll
// directly (including the server-streaming WatchJob).
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// The built SPA. Run `npm --prefix webui run build` (or the Dockerfile's node
// stage) to (re)generate dist/ before building the Go binary.
//
//go:embed all:dist
var dist embed.FS

// Handler serves the panel: static assets by exact path, and index.html for
// everything else (the SPA has no client-side routes, but this keeps deep
// links harmless).
func Handler() http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // embedded FS is compile-time; unreachable
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		data, err := fs.ReadFile(sub, p)
		if err != nil {
			// Unknown path → the SPA entry (client routing).
			data, err = fs.ReadFile(sub, "index.html")
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		} else {
			w.Header().Set("Content-Type", contentType(p))
		}
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(data)
	})
}

func contentType(p string) string {
	switch {
	case strings.HasSuffix(p, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(p, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(p, ".js"):
		return "text/javascript; charset=utf-8"
	case strings.HasSuffix(p, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(p, ".png"):
		return "image/png"
	case strings.HasSuffix(p, ".woff2"):
		return "font/woff2"
	default:
		return "application/octet-stream"
	}
}
