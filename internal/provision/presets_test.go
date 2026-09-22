package provision

import (
	"context"
	"encoding/json"
	"testing"
)

// fakeKV is an in-memory KV for pruneIndex.
type fakeKV struct {
	vals map[string]string
}

func (f *fakeKV) KVGet(_ context.Context, _ string, key string) (string, error) {
	return f.vals[key], nil
}
func (f *fakeKV) KVPut(_ context.Context, _ string, key, value string, _ int64) error {
	f.vals[key] = value
	return nil
}

func TestPruneIndexRemovesId(t *testing.T) {
	kv := &fakeKV{vals: map[string]string{
		"t.acme.__ids__": `["admin","maintainer","developer","planner","explorer"]`,
	}}
	if err := pruneIndex(context.Background(), kv, "acme", []string{"planner"}); err != nil {
		t.Fatal(err)
	}
	var ids []string
	if err := json.Unmarshal([]byte(kv.vals["t.acme.__ids__"]), &ids); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 4 {
		t.Fatalf("ids = %v, want 4 entries", ids)
	}
	for _, id := range ids {
		if id == "planner" {
			t.Fatal("planner was not pruned")
		}
	}
}

func TestPruneIndexNoopWhenAbsent(t *testing.T) {
	kv := &fakeKV{vals: map[string]string{}}
	if err := pruneIndex(context.Background(), kv, "acme", []string{"planner"}); err != nil {
		t.Fatal(err)
	}
	if len(kv.vals) != 0 {
		t.Fatal("absent index must not be created")
	}
}

func TestPresetCleanupDisabledWithoutURL(t *testing.T) {
	n, warns, err := PresetCleanup{Presets: []string{"planner"}}.Run(context.Background(), []string{"acme"})
	if err != nil || n != 0 || len(warns) != 0 {
		t.Fatalf("disabled cleanup must be a no-op: n=%d warns=%v err=%v", n, warns, err)
	}
}
