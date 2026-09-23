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
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"connectrpc.com/connect"

	wsv1connect "github.com/abcp-sdk/workspace-gateway/gen/workspace/v1/wsv1connect"
	"github.com/abcp-sdk/workspace-gateway/internal/agentclient"
	"github.com/abcp-sdk/workspace-gateway/internal/forgejo"
	"github.com/abcp-sdk/workspace-gateway/internal/idlewatch"
	"github.com/abcp-sdk/workspace-gateway/internal/imagebuild"
	"github.com/abcp-sdk/workspace-gateway/internal/members"
	"github.com/abcp-sdk/workspace-gateway/internal/provision"
	"github.com/abcp-sdk/workspace-gateway/internal/runtimeprofiles"
	"github.com/abcp-sdk/workspace-gateway/internal/sandboxmgr"
	"github.com/abcp-sdk/workspace-gateway/internal/sandboxreaper"
	"github.com/abcp-sdk/workspace-gateway/internal/servicesmgr"
	"github.com/abcp-sdk/workspace-gateway/internal/workspacesvc"

	natstransport "github.com/abcp-sdk/abc-protocol-go/v2/transport/nats"
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// envOrInt parses an int env var; unset/invalid falls back to def.
func envOrInt(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("warn: invalid int %s=%q, using %d", k, v, def)
		return def
	}
	return n
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
	registryHost := envOr("IMAGE_REGISTRY_HOST", "git.agent.svc.cluster.local")
	registryScheme := envOr("IMAGE_REGISTRY_SCHEME", "http")
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

	ac := agentclient.New(agentclient.Config{
		URL:          agentURL,
		ServiceToken: os.Getenv("GATEWAY_SERVICE_TOKEN"),
		AdminToken:   os.Getenv("AGENT_ADMIN_TOKEN"),
	})

	// Converge the agent to the deployment's expected state (idempotent):
	// per-tenant providers + force-set deployment-owned extension config. It
	// runs in the background so a slow/absent agent never blocks serving; a
	// failure is logged and retried on the next boot. Disable with
	// PROVISION_ON_BOOT=false.
	if envOr("PROVISION_ON_BOOT", "true") == "true" && os.Getenv("AGENT_ADMIN_TOKEN") != "" {
		go runProvision(agentURL)
	}
	svc := workspacesvc.New(workspacesvc.Deps{
		Agent:    ac.Raw(),
		Members:  store,
		Forgejo:  git,
		Sandbox:  sbx,
		Services: services,
		Builder: &imagebuild.Builder{
			Addr:           envOr("BUILDKIT_ADDR", "tcp://buildkitd.agent.svc.cluster.local:1234"),
			RegistryHost:   registryHost,
			RegistryScheme: registryScheme,
			DeriveRepo:     envOr("DERIVE_REPO", "root/sandbox"),
			WorkerBin:      envOr("WORKER_BIN", "/usr/local/lib/agent-worker/agent-worker"),
			RegistryUser:   os.Getenv("FORGEJO_USER"),
			RegistryPass:   os.Getenv("FORGEJO_PASSWORD"),
		},
		Runtime:             runtime,
		DefaultBase:         defaultBase,
		ToolchainOrg:        toolchainOrg,
		ServiceToken:        os.Getenv("GATEWAY_SERVICE_TOKEN"),
		ServiceTenant:       envOr("GATEWAY_SERVICE_TENANT", "workspace-extension"),
		GitCloneDir:         envOr("GIT_CLONE_DIR", "/data/git-clones"),
		SandboxNamespace:    sandboxNS,
		PublicServiceDomain: os.Getenv("PUBLIC_SERVICE_DOMAIN"),
		PreviewTTL:          envOrDuration("SERVICE_PREVIEW_TTL", 2*time.Hour),
		ServiceLogTail:      int64(envOrInt("SERVICE_LOG_TAIL", 500)),
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

	// Reclaim expired PREVIEW services (developer verification environments).
	if ttl := envOrDuration("SERVICE_PREVIEW_TTL", 2*time.Hour); ttl > 0 {
		interval := envOrDuration("SERVICE_PREVIEW_REAP_INTERVAL", 10*time.Minute)
		go func() {
			t := time.NewTicker(interval)
			defer t.Stop()
			for range t.C {
				if names, err := svc.ReapPreviewServices(context.Background()); err != nil {
					log.Printf("warn: preview reaper: %v", err)
				} else if len(names) > 0 {
					log.Printf("preview reaper: reclaimed %v", names)
				}
			}
		}()
	}

	// Nudge stalled sessions: a session that is idle, quiet for IDLEWATCH_AFTER
	// (default 1h) and still has unfinished todos gets a mailbox `trigger`.
	if envOr("IDLEWATCH_ENABLED", "true") == "true" {
		if natsURL := os.Getenv("NATS_URL"); natsURL != "" {
			if nbus, err := natstransport.Connect(natsURL); err != nil {
				log.Printf("warn: idlewatch: nats connect: %v", err)
			} else {
				watch := &idlewatch.Watchdog{
					Agent:        ac.Raw(),
					Admin:        ac.Admin(),
					AdminToken:   os.Getenv("AGENT_ADMIN_TOKEN"),
					ServiceToken: os.Getenv("GATEWAY_SERVICE_TOKEN"),
					Bus:          nbus,
					IdleAfter:    envOrDuration("IDLEWATCH_AFTER", time.Hour),
					Interval:     envOrDuration("IDLEWATCH_INTERVAL", 5*time.Minute),
					Cooldown:     envOrDuration("IDLEWATCH_COOLDOWN", 30*time.Minute),
				}
				go watch.Run(context.Background())
			}
		} else {
			log.Printf("idlewatch: disabled (NATS_URL unset)")
		}
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

// runProvision converges the agent (providers + deployment-owned config).
// Idempotent; failures are logged, never fatal (the agent may not be up yet).
func runProvision(agentURL string) {
	cfg := provision.Config{
		AgentURL:       agentURL,
		AdminToken:     os.Getenv("AGENT_ADMIN_TOKEN"),
		DefaultProfile: envOr("PROVISION_DEFAULT_PROFILE", "std"),
	}
	_ = json.Unmarshal([]byte(envOr("PROVISION_CALIBRATIONS", "[]")), &cfg.Calibrations)
	_ = json.Unmarshal([]byte(envOr("PROVISION_TENANT_PROFILE", "{}")), &cfg.TenantProfile)
	if raw := os.Getenv("PROVISION_RETIRED_PRESETS"); raw != "" {
		var ids []string
		if err := json.Unmarshal([]byte(raw), &ids); err == nil {
			cfg.PresetCleanup = provision.PresetCleanup{NATSURL: os.Getenv("NATS_URL"), Tenant: "*", Presets: ids}
		}
	}

	res, err := provision.Run(context.Background(), cfg)
	if err != nil {
		log.Printf("warn: provision: %v", err)
		return
	}
	log.Printf("provision: tenants=%v providers=%d calibrations=%d presets_removed=%d warnings=%d",
		res.Tenants, res.Providers, res.Calibrations, res.PresetsRemoved, len(res.Warnings))
	for _, w := range res.Warnings {
		log.Printf("warn: provision: %s", w)
	}
}
