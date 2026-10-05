package workspacesvc

import (
	"context"
	"fmt"
	"strings"
)

// ---- VM (Windows/macOS) sandbox defaults ----
//
// A VM sandbox boots a full OS in a KVM VM. Its guest disk is NOT baked into
// the image: the worker's start.sh sources golden-disk.sh, which fetches the
// base from GOLDEN_DISK_URL and creates a per-sandbox overlay. So the gateway
// picks the VM image, injects GOLDEN_DISK_URL, forces kvm, sets an 8Gi limit,
// and mounts a shared cache PVC so the base is downloaded once.

// defaultGoldenDiskCachePVC is the shared, platform-wide golden-disk cache PVC
// (single-node RWO). Overridable via SANDBOX_GOLDEN_PVC / SANDBOX_GOLDEN_PVC_SIZE.
const defaultGoldenDiskCachePVC = "sandbox-golden-cache"

// vmImageName is the canonical sandbox image (single per OS; the VARIANT — base
// vs xcode/devtools — is chosen by the `disk` URL, never by the tag).
var vmImageName = map[string]string{
	"macos":   "sandbox-macos:base",
	"windows": "sandbox-windows:base",
	// iOS runs INSIDE a macOS guest (Xcode + iOS Simulator), so it boots the
	// SAME golden disk as macos:xcode and shares the golden-disk cache PVC.
	"ios": "sandbox-ios:base",
}

// vmDefaultDisk maps os → its default golden-disk URL.
var vmDefaultDisk = map[string]string{
	"macos":   "http://artifact.worker.svc.cluster.local/artifacts/generic/golden-macos/15/data-xcode.qcow2",
	"windows": "http://artifact.worker.svc.cluster.local/artifacts/generic/golden-windows/11/data-devtools.qcow2",
	"ios":     "http://artifact.worker.svc.cluster.local/artifacts/generic/golden-macos/15/data-xcode.qcow2",
}

// deviceImageName maps a "device" os → its canonical sandbox image. Android
// boots an emulator inside a Linux container: kvm + extra memory, but NO golden
// disk (its guest is baked into the image). iOS is NOT here — it uses a golden
// disk (see vmImageName/vmDefaultDisk) because its macOS guest disk is external.
var deviceImageName = map[string]string{
	"android": "sandbox-android:aosp",
}

// normalizeOS maps a caller's `os` to a canonical value (default "linux").
func normalizeOS(os string) string {
	switch strings.ToLower(strings.TrimSpace(os)) {
	case "", "linux":
		return "linux"
	case "macos", "darwin", "osx":
		return "macos"
	case "windows", "win":
		return "windows"
	case "android":
		return "android"
	case "ios":
		return "ios"
	default:
		return "linux"
	}
}

// vmSandboxImage returns the full sandbox image ref for a VM os, using the
// deployment's sandbox registry host.
func (s *Service) vmSandboxImage(osName string) string {
	host := s.sandboxImageRegistryHost
	if host == "" {
		host = "artifact.worker.svc.cluster.local"
	}
	name := vmImageName[osName]
	if name == "" {
		name = deviceImageName[osName]
	}
	return host + "/sandbox/" + name
}

// withEnv returns a copy of base with extra's keys set (extra wins).
func withEnv(base, extra map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// ensureGoldenDiskCache creates the shared cache PVC if absent (idempotent).
func (s *Service) ensureGoldenDiskCache(ctx context.Context, name string) error {
	size := s.goldenDiskCacheSize
	if size == "" {
		size = "40Gi"
	}
	if _, err := s.services.CreatePVC(ctx, name, size, s.pvcStorageClass, ""); err != nil {
		return fmt.Errorf("ensure golden-disk cache pvc %q: %w", name, err)
	}
	return nil
}
