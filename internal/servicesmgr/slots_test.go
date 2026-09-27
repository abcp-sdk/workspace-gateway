package servicesmgr

import (
	"context"
	"testing"
)

func TestDeploySlotAndPromote(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()

	// Deploy blue (default) — the router targets blue.
	svc, err := c.DeploySlot(ctx, Spec{Name: "web", Image: "img:blue", Creator: "t", ContainerPort: 8080, Slot: SlotBlue, Ports: []Port{{Port: 80, Protocol: "tcp", TargetPort: 8080}}})
	if err != nil {
		t.Fatalf("deploy blue: %v", err)
	}
	if svc.ActiveSlot != SlotBlue {
		t.Fatalf("active = %q, want blue", svc.ActiveSlot)
	}
	if len(svc.Slots) != 1 || svc.Slots[0].Slot != SlotBlue {
		t.Fatalf("slots = %+v", svc.Slots)
	}

	// Deploy green — router STAYS on blue (no auto-promote).
	svc, err = c.DeploySlot(ctx, Spec{Name: "web", Image: "img:green", Creator: "t", ContainerPort: 8080, Slot: SlotGreen, Ports: []Port{{Port: 80, Protocol: "tcp", TargetPort: 8080}}})
	if err != nil {
		t.Fatalf("deploy green: %v", err)
	}
	if svc.ActiveSlot != SlotBlue {
		t.Fatalf("active = %q, want blue (green must not auto-promote)", svc.ActiveSlot)
	}
	if len(svc.Slots) != 2 {
		t.Fatalf("slots = %+v, want 2", svc.Slots)
	}
	if svc.Image != "img:blue" {
		t.Fatalf("active image = %q, want img:blue", svc.Image)
	}

	// Promote requires readiness by default (the fake reports 0 ready replicas).
	if _, err = c.Promote(ctx, "web", true); err == nil {
		t.Fatal("expected promote to require ready (0 ready replicas)")
	}
	// force=true (requireReady=false) promotes anyway.
	svc, err = c.Promote(ctx, "web", false)
	if err != nil {
		t.Fatalf("force promote: %v", err)
	}
	if svc.ActiveSlot != SlotGreen || svc.Image != "img:green" {
		t.Fatalf("after promote: active=%q image=%q", svc.ActiveSlot, svc.Image)
	}

	// Rollback flips back to blue.
	svc, err = c.Rollback(ctx, "web")
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if svc.ActiveSlot != SlotBlue || svc.Image != "img:blue" {
		t.Fatalf("after rollback: active=%q image=%q", svc.ActiveSlot, svc.Image)
	}

	// List shows ONE row for the blue-green service.
	list, err := c.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v, %v (want 1 blue-green row)", list, err)
	}
	if list[0].ActiveSlot != SlotBlue || len(list[0].Slots) != 2 {
		t.Fatalf("list row = %+v", list[0])
	}

	// Delete removes router + both slots.
	if ok, err := c.Delete(ctx, "web"); err != nil || !ok {
		t.Fatalf("delete: %v %v", ok, err)
	}
	if list, _ := c.List(ctx); len(list) != 0 {
		t.Fatalf("after delete list = %+v", list)
	}
}

func TestDeploySlotValidation(t *testing.T) {
	c := newTestClient()
	if _, err := c.DeploySlot(context.Background(), Spec{Name: "x", Image: "i", Slot: "purple"}); err == nil {
		t.Fatal("expected invalid slot error")
	}
}

func TestPlainServiceNotSlotted(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if _, err := c.Deploy(ctx, Spec{Name: "plain", Image: "img:1", Creator: "t", ContainerPort: 8080}); err != nil {
		t.Fatal(err)
	}
	svc, err := c.Get(ctx, "plain")
	if err != nil {
		t.Fatal(err)
	}
	if svc.ActiveSlot != "" || len(svc.Slots) != 0 {
		t.Fatalf("plain service should have no slots: %+v", svc)
	}
}
