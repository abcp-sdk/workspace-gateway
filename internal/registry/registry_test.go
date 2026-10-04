package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestListImages exercises catalog + tags listing, owner filtering, pagination
// via the Link header, digest pseudo-tag skipping and Basic auth.
func TestListImages(t *testing.T) {
	mux := http.NewServeMux()
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		u, p, ok := r.BasicAuth()
		if !ok || u != "root" || p != "tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return false
		}
		return true
	}
	mux.HandleFunc("/v2/_catalog", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		if r.URL.Query().Get("last") == "" {
			// First page: advertise a next link.
			w.Header().Set("Link", `</v2/_catalog?n=2&last=agent-toolchain/nats>; rel="next"`)
			json.NewEncoder(w).Encode(map[string]any{"repositories": []string{"agent-toolchain/nats", "agent-toolchain/toolchain-go"}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"repositories": []string{"sandbox/sandbox-base"}})
	})
	mux.HandleFunc("/v2/agent-toolchain/toolchain-go/tags/list", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"name": "agent-toolchain/toolchain-go", "tags": []string{"debian-trixie", "sha256:abc"}})
	})
	mux.HandleFunc("/v2/agent-toolchain/nats/tags/list", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"name": "agent-toolchain/nats", "tags": []string{}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &Client{Host: strings.TrimPrefix(srv.URL, "http://"), Scheme: "http", User: "root", Pass: "tok"}
	imgs, err := c.ListImages(context.Background(), "agent-toolchain", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) != 1 || imgs[0].Name != "toolchain-go" || imgs[0].Tag != "debian-trixie" {
		t.Fatalf("unexpected images: %+v", imgs)
	}
}

// TestListImagesUnauthorized confirms a missing credential is surfaced.
func TestListImagesUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := &Client{Host: strings.TrimPrefix(srv.URL, "http://"), Scheme: "http"}
	if _, err := c.ListImages(context.Background(), "x", ""); err == nil {
		t.Fatal("expected error on 401")
	}
}
