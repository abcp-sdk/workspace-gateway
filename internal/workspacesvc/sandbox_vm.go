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

// vmImageName is the canonical sandbox IMAGE per OS (NO tag). The tag comes from
// the deployment (SANDBOX_VM_IMAGE_TAG / Deps.VMImageTag) so the mapping never
// depends on a mutable alias like `base`, which a registry cleanup can prune.
// (The VARIANT — base vs xcode/devtools — is chosen by the `disk` URL, never by
// the tag.)
var vmImageName = map[string]string{
	"macos":   "sandbox-macos",
	"windows": "sandbox-windows",
}

// vmDefaultDisk maps os → its default golden-disk URL.
var vmDefaultDisk = map[string]string{
	"macos":   "http://artifact.worker.svc.cluster.local/artifacts/generic/golden-macos/15/data-xcode.qcow2",
	"windows": "http://artifact.worker.svc.cluster.local/artifacts/generic/golden-windows/11/data-devtools.qcow2",
}

// deviceImageName maps a "device" os → its canonical sandbox image WITH its own
// tag. Unlike the VM images (whose variant is chosen by the `disk` URL), a
// device image's variant IS its tag (android: aosp vs gms), so it keeps its own
// tag rather than the VM image tag.
//
// Android boots an emulator inside a Linux container: kvm + extra memory, but
// NO golden disk (its guest is baked into the image).
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
	default:
		return "linux"
	}
}

// vmImageTagOrDefault is the tag applied to VM sandbox images (macos/windows).
// Device images carry their own tag (see deviceImageName).
// Deployment-pinned (Deps.VMImageTag, env SANDBOX_VM_IMAGE_TAG); "base" default.
func (s *Service) vmImageTagOrDefault() string {
	if s.vmImageTag != "" {
		return s.vmImageTag
	}
	return "base"
}

// vmSandboxImage returns the full sandbox image ref for a VM/device os.
// VM images (macos/windows) get the deployment-pinned VM tag; a device
// image (android) keeps its own tag (deviceImageName already carries it).
func (s *Service) vmSandboxImage(osName string) string {
	host := s.sandboxImageRegistryHost
	if host == "" {
		host = "artifact.worker.svc.cluster.local"
	}
	if name := vmImageName[osName]; name != "" {
		return host + "/sandbox/" + name + ":" + s.vmImageTagOrDefault()
	}
	return host + "/sandbox/" + deviceImageName[osName]
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
	if _, err := s.services.CreatePVC(ctx, name, size, s.pvcStorageClass, "", ""); err != nil {
		return fmt.Errorf("ensure golden-disk cache pvc %q: %w", name, err)
	}
	return nil
}
