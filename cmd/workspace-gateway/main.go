// Command workspace-gateway fronts the abc agent for the workspace webui.
//
// It is the SINGLE trusted entry point for creating sessions: it derives a
// session's role from its `org:repo:branch` name, binds the immutable preset,
// and owns the tenant's repo visibility. It also provides read-only git browse
// (Forgejo) and sandbox lifecycle/browse (via worker-manager).
//
// The webui speaks ONLY workspace.v1. The agent.v1 handler is deliberately NOT
// mounted: the gateway forwards a minimal, policy-free subset (chat, models,
// config, providers, files) through its own service.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"connectrpc.com/connect"

	wsv1connect "github.com/abcp-sdk/workspace-gateway/gen/workspace/v1/wsv1connect"
	"github.com/abcp-sdk/workspace-gateway/internal/agentclient"
	"github.com/abcp-sdk/workspace-gateway/internal/forgejo"
	"github.com/abcp-sdk/workspace-gateway/internal/imagebuild"
	"github.com/abcp-sdk/workspace-gateway/internal/members"
	"github.com/abcp-sdk/workspace-gateway/internal/runtimeprofiles"
	"github.com/abcp-sdk/workspace-gateway/internal/sandboxmgr"
	"github.com/abcp-sdk/workspace-gateway/internal/sandboxreaper"
	"github.com/abcp-sdk/workspace-gateway/internal/servicesmgr"
	"github.com/abcp-sdk/workspace-gateway/internal/workspacesvc"
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// envOrDuration parses a duration env var (Go syntax, e.g. "24h", "10m"); an
// unset/invalid value falls back to def.
func envOrDuration(k string, def time.Duration) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("warn: invalid duration %s=%q, using %s", k, v, def)
		return def
	}
	return d
}

func main() {
	listen := "0.0.0.0:" + envOr("PORT", "8080")
	agentURL := envOr("AGENT_URL", "http://standalone-agent:80")
	gitURL := envOr("FORGEJO_URL", "")
	gitToken := os.Getenv("FORGEJO_TOKEN")
	db := envOr("GATEWAY_DB", "workspace-gateway.db")
	sandboxNS := envOr("SANDBOX_NAMESPACE", "worker")
	registryHost := envOr("IMAGE_REGISTRY_HOST", "git.agent.fenjin.org")
	toolchainOrg := envOr("TOOLCHAIN_ORG", "agent-toolchain")
	defaultBase := envOr("DEFAULT_BASE_IMAGE", registryHost+"/"+toolchainOrg+"/toolchain-base:debian-trixie")

	store, err := members.Open(db)
	if err != nil {
		log.Fatalf("open members db: %v", err)
	}
	defer store.Close()

	// The gateway owns the sandbox + service lifecycle in-process (worker-manager
	// folded in). A k8s failure is non-fatal: git/session routes still work.
	sbx, err := sandboxmgr.New(sandboxmgr.Config{Namespace: sandboxNS})
	if err != nil {
		log.Fatalf("sandbox backend: %v", err)
	}
	services, err := servicesmgr.New(servicesmgr.Config{Namespace: sandboxNS})
	if err != nil {
		log.Fatalf("service backend: %v", err)
	}
	runtime, err := runtimeprofiles.Load(os.Getenv("WORKSPACE_RUNTIME"))
	if err != nil {
		log.Fatalf("load runtime settings: %v", err)
	}

	git := forgejo.New(gitURL, gitToken)
	// Ensure the toolchain org exists (idempotent). Non-fatal: a Forgejo blip
	// must not stop the gateway from serving sessions.
	if err := git.EnsureOrg(context.Background(), toolchainOrg); err != nil {
		log.Printf("warn: ensure toolchain org %q: %v", toolchainOrg, err)
	}

	ac := agentclient.New(agentURL)
	svc := workspacesvc.New(workspacesvc.Deps{
		Agent:    ac.Raw(),
		Members:  store,
		Forgejo:  git,
		Sandbox:  sbx,
		Services: services,
		Builder: &imagebuild.Builder{
			Addr:         envOr("BUILDKIT_ADDR", "tcp://buildkitd.agent.svc.cluster.local:1234"),
			RegistryHost: registryHost,
			DeriveRepo:   envOr("DERIVE_REPO", "root/sandbox"),
			WorkerBin:    envOr("WORKER_BIN", "/usr/local/lib/easyworker/easyworker"),
			RegistryUser: os.Getenv("FORGEJO_USER"),
			RegistryPass: os.Getenv("FORGEJO_PASSWORD"),
		},
		Runtime:       runtime,
		DefaultBase:   defaultBase,
		ToolchainOrg:  toolchainOrg,
		ServiceToken:  os.Getenv("GATEWAY_SERVICE_TOKEN"),
		ServiceTenant: envOr("GATEWAY_SERVICE_TENANT", "workspace-extension"),
	})

	// Reclaim idle sandboxes: no worker jobs for SANDBOX_IDLE_TTL (default 24h).
	if idle := envOrDuration("SANDBOX_IDLE_TTL", 24*time.Hour); idle > 0 {
		reaper := &sandboxreaper.Reaper{
			Sbx:      sbx,
			IdleTTL:  idle,
			Interval: envOrDuration("SANDBOX_REAP_INTERVAL", 10*time.Minute),
		}
		go reaper.Run(context.Background())
	}

	mux := http.NewServeMux()
	// workspace.v1 is the gateway's ONLY surface.
	wpath, whandler := wsv1connect.NewBranchSessionServiceHandler(svc,
		connect.WithInterceptors())
	mux.Handle(wpath, whandler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{
		Addr:              listen,
		Handler:           mux,
		Protocols:         protocols,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("workspace-gateway listening on %s (agent=%s forgejo=%s sandbox-ns=%s)", listen, agentURL, gitURL, sandboxNS)
	log.Fatal(srv.ListenAndServe())
}
