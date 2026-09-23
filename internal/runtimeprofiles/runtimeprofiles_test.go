package runtimeprofiles

import (
	"os"
	"testing"
)

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
	// A GPU request MUST select the NVIDIA runtime class, or the driver devices
	// are never mounted.
	if got := s.Render(false, 1).RuntimeClass; got != "nvidia" {
		t.Fatalf("gpu runtime class = %q, want nvidia", got)
	}
}

func TestRenderKVMHasNoRuntimeClassByDefault(t *testing.T) {
	if got := DefaultSettings().Render(true, 0).RuntimeClass; got != "" {
		t.Fatalf("kvm default runtime class = %q, want empty", got)
	}
}

func TestRenderGPUPlusKVM(t *testing.T) {
	r := DefaultSettings().Render(true, 2)
	if r.DeviceLimits["nvidia.com/gpu"] != "2" || r.DeviceLimits["squat.ai/kvm"] != "1" || !r.NeedsTun {
		t.Fatalf("gpu+kvm render: %+v", r)
	}
	if r.RuntimeClass != "nvidia" {
		t.Fatalf("gpu+kvm runtime class = %q, want nvidia", r.RuntimeClass)
	}
}

func TestLoadOverrideRuntimeClass(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/r.json"
	if err := os.WriteFile(path, []byte(`{"gpuRuntimeClass":"nvidia-gpu","kvmRuntimeClass":"kvm"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.GPURuntimeClass != "nvidia-gpu" || s.KVMRuntimeClass != "kvm" {
		t.Fatalf("override not applied: %+v", s)
	}
	if got := s.Render(true, 1).RuntimeClass; got != "nvidia-gpu" {
		t.Fatalf("gpu wins when both requested: %q", got)
	}
	if got := s.Render(true, 0).RuntimeClass; got != "kvm" {
		t.Fatalf("kvm-only runtime class = %q", got)
	}
}

func TestLoadOverride(t *testing.T) {
	// Missing file -> defaults.
	s, err := Load("/nonexistent/runtimes.json")
	if err != nil || s.KVMDevice != "squat.ai/kvm" {
		t.Fatalf("defaults: %+v err=%v", s, err)
	}
}
