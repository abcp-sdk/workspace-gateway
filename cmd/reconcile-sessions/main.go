// Command reconcile-sessions is a ONE-SHOT migration: it walks every repo the
// tenant owns and ensures each of its Forgejo branches has the corresponding
// `org:repo:branch` branch session (repo:branch <-> session, 1:1).
//
// It exists to backfill sessions for branches that predate the auto-ensure
// paths (UI + agent tools). It is IDEMPOTENT and safe to re-run: existing
// sessions are returned unchanged.
//
// It talks to the gateway's OWN BranchSessionService over Connect (h2c for an
// in-cluster address, h1 for a local port-forward), authenticating with a
// TENANT token (the gateway derives the tenant from it).
//
// Usage:
//
//	GATEWAY_URL=http://localhost:8080 \
//	GATEWAY_TOKEN=<tenant-token> \
//	  go run ./cmd/reconcile-sessions
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"

	wsv1 "github.com/abcp-sdk/workspace-gateway/gen/workspace/v1"
	wsv1connect "github.com/abcp-sdk/workspace-gateway/gen/workspace/v1/wsv1connect"
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	base := strings.TrimRight(envOr("GATEWAY_URL", "http://localhost:8080"), "/")
	token := os.Getenv("GATEWAY_TOKEN")
	if token == "" {
		log.Fatal("GATEWAY_TOKEN is required (a tenant token)")
	}

	// h2c so the same binary works against the in-cluster Service; a plain
	// HTTP/1 port-forward also works because UnencryptedHTTP2 + HTTP1 are both
	// enabled.
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	hc := &http.Client{Transport: &http.Transport{Protocols: p}}

	interceptor := connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Authorization", "Bearer "+token)
			return next(ctx, req)
		}
	})
	c := wsv1connect.NewBranchSessionServiceClient(hc, base, connect.WithInterceptors(interceptor))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	repos, err := c.ListRepos(ctx, connect.NewRequest(&wsv1.ListReposRequest{}))
	if err != nil {
		log.Fatalf("ListRepos: %v", err)
	}

	var created, existing, failed int
	for _, r := range repos.Msg.GetRepos() {
		br, err := c.Branches(ctx, connect.NewRequest(&wsv1.BranchesRequest{Org: r.GetOrg(), Repo: r.GetRepo()}))
		if err != nil {
			log.Printf("warn: branches %s/%s: %v", r.GetOrg(), r.GetRepo(), err)
			failed++
			continue
		}
		for _, b := range br.Msg.GetBranches() {
			res, err := c.EnsureBranchSession(ctx, connect.NewRequest(&wsv1.EnsureBranchSessionRequest{
				Org:    r.GetOrg(),
				Repo:   r.GetRepo(),
				Branch: b.GetName(),
			}))
			if err != nil {
				log.Printf("warn: ensure %s/%s:%s: %v", r.GetOrg(), r.GetRepo(), b.GetName(), err)
				failed++
				continue
			}
			// EnsureBranchSession is idempotent; we cannot tell created vs
			// existing from the response, so report the total ensured.
			_ = res
			created++
		}
	}
	fmt.Printf("reconcile-sessions: repos=%d ensured=%d failed=%d (idempotent)\n",
		len(repos.Msg.GetRepos()), created, failed)
	if failed > 0 {
		os.Exit(1)
	}
	_ = existing
}
