// Command provision converges a workspace agent to the deployment's expected
// state: per-tenant providers (so sessions can run) and deployment-owned
// extension config (so a stale value cannot win over the chart).
//
// It is IDEMPOTENT and safe to run on every upgrade / as a post-install Job.
//
// Usage:
//
//	AGENT_URL=http://workspace-agent.agent.svc.cluster.local \
//	AGENT_ADMIN_TOKEN=dev-admin-token \
//	  go run ./cmd/provision
//
// Optional env:
//
//	PROVISION_CALIBRATIONS  JSON [{tenant,extId,name,value}] force-set values
//	                        (tenant "*" = every known tenant).
//	PROVISION_TENANT_PROFILE JSON {"myuser":"full"} per-tenant profile override.
//	PROVISION_DEFAULT_PROFILE profile for tenants without an override (default "std").
package main

import (
	"context"
	"encoding/json"
	"log"
	"os"

	"github.com/abcp-sdk/workspace-gateway/internal/provision"
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	cfg := provision.Config{
		AgentURL:       envOr("AGENT_URL", "http://workspace-agent.agent.svc.cluster.local"),
		AdminToken:     os.Getenv("AGENT_ADMIN_TOKEN"),
		DefaultProfile: envOr("PROVISION_DEFAULT_PROFILE", "std"),
	}
	if cfg.AdminToken == "" {
		log.Fatal("AGENT_ADMIN_TOKEN is required")
	}
	if raw := os.Getenv("PROVISION_CALIBRATIONS"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cfg.Calibrations); err != nil {
			log.Fatalf("PROVISION_CALIBRATIONS: %v", err)
		}
	}
	if raw := os.Getenv("PROVISION_TENANT_PROFILE"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cfg.TenantProfile); err != nil {
			log.Fatalf("PROVISION_TENANT_PROFILE: %v", err)
		}
	}
	if raw := os.Getenv("PROVISION_RETIRED_PRESETS"); raw != "" {
		var ids []string
		if err := json.Unmarshal([]byte(raw), &ids); err != nil {
			log.Fatalf("PROVISION_RETIRED_PRESETS: %v", err)
		}
		cfg.PresetCleanup = provision.PresetCleanup{NATSURL: os.Getenv("NATS_URL"), Tenant: "*", Presets: ids}
	}

	res, err := provision.Run(context.Background(), cfg)
	if err != nil {
		log.Fatalf("provision: %v", err)
	}
	log.Printf("provision: tenants=%v providers=%d calibrations=%d presets_removed=%d warnings=%d",
		res.Tenants, res.Providers, res.Calibrations, res.PresetsRemoved, len(res.Warnings))
	for _, w := range res.Warnings {
		log.Printf("warn: %s", w)
	}
	if len(res.Warnings) > 0 {
		os.Exit(1)
	}
}
