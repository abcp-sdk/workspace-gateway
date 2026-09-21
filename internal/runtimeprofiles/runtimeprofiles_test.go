package runtimeprofiles

import "testing"

func TestRenderPlain(t *testing.T) {
	r := DefaultSettings().Render(false, 0)
	if r.NeedsTun || len(r.DeviceLimits) != 0 || r.SecurityContext != nil {
		t.Fatalf("plain render not empty: %+v", r)
	}
}

func TestRenderKVM(t *testing.T) {
	r := DefaultSettings().Render(true, 0)
	if !r.NeedsTun || r.DeviceLimits["squat.ai/kvm"] != "1" {
		t.Fatalf("kvm render: %+v", r)
	}
	if r.SecurityContext == nil || r.SecurityContext.Privileged == nil || *r.SecurityContext.Privileged {
		t.Fatal("kvm must never be privileged")
	}
}

func TestRenderGPU(t *testing.T) {
	s := DefaultSettings()
	if got := s.Render(false, 3).DeviceLimits["nvidia.com/gpu"]; got != "3" {
		t.Fatalf("gpu count = %q", got)
	}
	if _, ok := s.Render(false, 0).DeviceLimits["nvidia.com/gpu"]; ok {
		t.Fatal("gpu requested with count 0")
	}
}

func TestRenderGPUPlusKVM(t *testing.T) {
	r := DefaultSettings().Render(true, 2)
	if r.DeviceLimits["nvidia.com/gpu"] != "2" || r.DeviceLimits["squat.ai/kvm"] != "1" || !r.NeedsTun {
		t.Fatalf("gpu+kvm render: %+v", r)
	}
}

func TestLoadOverride(t *testing.T) {
	// Missing file -> defaults.
	s, err := Load("/nonexistent/runtimes.json")
	if err != nil || s.KVMDevice != "squat.ai/kvm" {
		t.Fatalf("defaults: %+v err=%v", s, err)
	}
}
