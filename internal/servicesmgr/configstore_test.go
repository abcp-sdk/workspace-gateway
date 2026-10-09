package servicesmgr

import (
	"context"
	"testing"
)

func TestConfigMapUpsertA1(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if _, err := c.PutConfigMap(ctx, "cfg", map[string]string{"a": "1"}, "alice", "acme"); err != nil {
		t.Fatalf("put: %v", err)
	}
	// Another tenant cannot overwrite.
	if _, err := c.PutConfigMap(ctx, "cfg", map[string]string{"a": "2"}, "bob", ""); err == nil {
		t.Fatal("cross-tenant overwrite must fail")
	}
	// Same creator may update.
	if _, err := c.PutConfigMap(ctx, "cfg", map[string]string{"a": "3"}, "alice", "acme"); err != nil {
		t.Fatalf("same-tenant update: %v", err)
	}
}

func TestSecretUpsertA1(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if _, err := c.PutSecret(ctx, "sec", map[string]string{"k": "v"}, "alice", "acme"); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := c.PutSecret(ctx, "sec", map[string]string{"k": "x"}, "bob", ""); err == nil {
		t.Fatal("cross-tenant overwrite must fail")
	}
}
