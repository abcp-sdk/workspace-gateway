package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	natstransport "github.com/abcp-sdk/abc-protocol-go/v2/transport/nats"
)

// NATS bucket + key shapes for presets (mirrors the agent's kv-store).
const (
	presetsBucket  = "abc-presets"
	presetIndexKey = "__ids__"
)

// PresetCleanup removes RETIRED system presets from every tenant's KV bucket.
//
// The agent seeds system presets create-if-absent and REFUSES to delete a
// system preset via its API (isSystem guard), so a preset retired from
// SYSTEM_PRESETS_FILE would otherwise linger forever. This deletes it directly
// from the JetStream KV bucket and prunes the per-tenant index. Idempotent.
type PresetCleanup struct {
	// NATSURL is the agent-account NATS URL. Empty disables cleanup.
	NATSURL string
	// Tenant is "*" (every known tenant) or one tenant id.
	Tenant string
	// Presets are the ids to remove.
	Presets []string
	// Timeout bounds the whole cleanup. Zero = 30s.
	Timeout time.Duration
}

// Run removes the configured presets. Best-effort per key; a failure is a
// warning, never fatal.
func (c PresetCleanup) Run(ctx context.Context, known []string) (int, []string, error) {
	if c.NATSURL == "" || len(c.Presets) == 0 {
		return 0, nil, nil
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	bus, err := natstransport.Connect(c.NATSURL)
	if err != nil {
		return 0, nil, fmt.Errorf("nats connect: %w", err)
	}
	defer bus.Close()

	tenants := resolveTenants(c.Tenant, known)
	removed := 0
	var warnings []string
	for _, tenant := range tenants {
		for _, id := range c.Presets {
			key := fmt.Sprintf("t.%s.%s", tenant, id)
			if err := bus.KVDelete(ctx, presetsBucket, key); err != nil {
				// A missing key is fine (idempotent re-run).
				warnings = append(warnings, fmt.Sprintf("preset cleanup %s/%s: %v", tenant, id, err))
				continue
			}
			removed++
		}
		if err := pruneIndex(ctx, bus, tenant, c.Presets); err != nil {
			warnings = append(warnings, fmt.Sprintf("preset index %s: %v", tenant, err))
		}
	}
	return removed, warnings, nil
}

// pruneIndex removes the given ids from the per-tenant preset index.
func pruneIndex(ctx context.Context, bus interface {
	KVGet(context.Context, string, string) (string, error)
	KVPut(context.Context, string, string, string, int64) error
}, tenant string, drop []string) error {
	indexKey := fmt.Sprintf("t.%s.%s", tenant, presetIndexKey)
	raw, err := bus.KVGet(ctx, presetsBucket, indexKey)
	if err != nil || raw == "" {
		return nil // no index -> nothing to prune
	}
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil // unknown shape -> leave it
	}
	dropSet := map[string]bool{}
	for _, d := range drop {
		dropSet[d] = true
	}
	kept := ids[:0]
	for _, id := range ids {
		if !dropSet[id] {
			kept = append(kept, id)
		}
	}
	if len(kept) == len(ids) {
		return nil // nothing dropped
	}
	out, err := json.Marshal(kept)
	if err != nil {
		return err
	}
	return bus.KVPut(ctx, presetsBucket, indexKey, string(out), 0)
}
