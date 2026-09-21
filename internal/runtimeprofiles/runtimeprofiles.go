// Package runtimeprofiles renders a workload's runtime capabilities. The
// caller picks only two orthogonal knobs — KVM on/off and a GPU count — and the
// gateway maps them to device requests, mounts, caps and resources. Callers
// never supply raw pod settings, so the security boundary is the deployment's
// Settings (overridable via a JSON env file).
package runtimeprofiles

import (
	"encoding/json"
	"os"

	corev1 "k8s.io/api/core/v1"
)

// Settings holds the deployment's runtime knobs.
type Settings struct {
	// KVMDevice is the extended resource the device plugin exposes for /dev/kvm.
	KVMDevice string `json:"kvmDevice"`
	// GPUDevice is the extended resource for NVIDIA GPUs.
	GPUDevice string `json:"gpuDevice"`
	// KVMNeedsTun mounts /dev/net/tun (VMs/tunnels need it).
	KVMNeedsTun bool `json:"kvmNeedsTun"`
	// KVMSecurityContext applied to a KVM container (privileged is ALWAYS
	// forced false by Render).
	KVMSecurityContext *corev1.SecurityContext `json:"kvmSecurityContext"`
}

// DefaultSettings are the built-in knobs (matching the cluster's device plugin
// and the easyworker VM profiles).
func DefaultSettings() Settings {
	return Settings{
		KVMDevice:   "squat.ai/kvm",
		GPUDevice:   "nvidia.com/gpu",
		KVMNeedsTun: true,
		KVMSecurityContext: &corev1.SecurityContext{
			RunAsUser:                int64Ptr(0),
			AllowPrivilegeEscalation: boolPtr(true),
			Privileged:               boolPtr(false),
			Capabilities: &corev1.Capabilities{Add: []corev1.Capability{
				"NET_ADMIN", "NET_RAW", "SYS_ADMIN", "SYS_NICE", "MKNOD",
				"CHOWN", "SETUID", "SETGID", "DAC_OVERRIDE", "FOWNER", "KILL",
			}},
			AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined},
			SeccompProfile:  &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
		},
	}
}

// Load layers a JSON override file (env WORKSPACE_RUNTIME) over the defaults.
// A missing path yields the defaults.
func Load(path string) (Settings, error) {
	s := DefaultSettings()
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return s, err
	}
	var o Settings
	if err := json.Unmarshal(b, &o); err != nil {
		return s, err
	}
	if o.KVMDevice != "" {
		s.KVMDevice = o.KVMDevice
	}
	if o.GPUDevice != "" {
		s.GPUDevice = o.GPUDevice
	}
	if o.KVMNeedsTun {
		s.KVMNeedsTun = true
	}
	if o.KVMSecurityContext != nil {
		s.KVMSecurityContext = o.KVMSecurityContext
	}
	return s, nil
}

// Rendered is the pod-level result.
type Rendered struct {
	NeedsTun         bool
	DeviceLimits     map[string]string
	NodeSelector     map[string]string
	ImagePullSecrets []string
	Env              map[string]string
	SecurityContext  *corev1.SecurityContext
	Resources        *corev1.ResourceRequirements
}

// Render maps (kvm, gpuCount) to pod settings. gpuCount > 0 requests that many
// GPUs. The returned SecurityContext always has Privileged=false.
func (s Settings) Render(kvm bool, gpuCount int) Rendered {
	r := Rendered{}
	if gpuCount > 0 {
		r.DeviceLimits = map[string]string{s.GPUDevice: itoa(gpuCount)}
	}
	if kvm {
		r.NeedsTun = s.KVMNeedsTun
		if r.DeviceLimits == nil {
			r.DeviceLimits = map[string]string{}
		}
		r.DeviceLimits[s.KVMDevice] = "1"
		if s.KVMSecurityContext != nil {
			sc := *s.KVMSecurityContext
			priv := false
			sc.Privileged = &priv
			r.SecurityContext = &sc
		}
	}
	return r
}

func int64Ptr(i int64) *int64 { return &i }
func boolPtr(b bool) *bool    { return &b }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
