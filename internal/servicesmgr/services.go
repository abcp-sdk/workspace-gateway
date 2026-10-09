// Package servicesmgr manages long-lived services: a user image run as a
// Kubernetes Deployment + Service in the managed namespace. Unlike sandboxes,
// services get NO worker injection and NO sidecar — they are the caller's app.
package servicesmgr

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/yaml"

	"github.com/abcp-sdk/workspace-gateway/internal/runtimeprofiles"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"os"
)

// Labels/annotations stamped on every service object.
const (
	LabelManaged = "workspace/service"
	LabelName    = "workspace/service-name"
	AnnoImage    = "workspace/service-image"
	AnnoCreator  = "workspace/service-creator"
	// AnnoSession binds a service to the session that deployed it, so the webui
	// can show which session owns it (services stay tenant-visible).
	AnnoSession = "workspace/service-session"
	// AnnoNamespace records the tenant-internal namespace (org segment of the
	// creating session).
	AnnoNamespace = "workspace/service-namespace"
	// AnnoReplicasBeforePause remembers the replica count in effect when a
	// service was paused (scaled to 0), so Resume can restore it. Presence of
	// this annotation is what marks a service as PAUSED.
	AnnoReplicasBeforePause = "workspace/service-replicas-before-pause"
	// PVC labels/annotations stamped on every claim this manager creates.
	LabelPVC       = "workspace/pvc"
	LabelPVCName   = "workspace/pvc-name"
	AnnoPVCCreator = "workspace/pvc-creator"
	// AnnoPVCNamespace records the tenant-internal namespace of a claim.
	AnnoPVCNamespace = "workspace/pvc-namespace"
)

// VolumeMount binds a named PVC into a service container.
type VolumeMount struct {
	PVC       string
	MountPath string
	ReadOnly  bool
	SubPath   string
}

// PVC is a live PersistentVolumeClaim view.
type PVC struct {
	Name         string
	Size         string
	StorageClass string
	Phase        string
	Creator      string
	// Namespace is the tenant-internal namespace (org segment of the session).
	Namespace string
	CreatedAt int64
	// MountedBy lists the managed Deployments in the namespace that mount this
	// claim (best effort).
	MountedBy []string
}

// Service is a live service view.
type Service struct {
	Name     string
	Image    string
	Phase    string
	Ready    bool
	Replicas int32
	URL      string
	Creator  string
	Session  string
	// Namespace is the tenant-internal namespace (org segment of the session).
	Namespace string
	CreatedAt int64
	// Ports is every port this service exposes (across its primary Service and
	// any sibling `<name>-<suffix>` Services).
	Ports []Port
	// Paused is true when the service was scaled to zero by Pause (Replicas
	// then reads 0). Resume restores the pre-pause replica count.
	Paused bool
	// Pod diagnostics (best-effort; zero when no pod is observed).
	PodPhase string
	Restarts int32
	// Message is the waiting/terminated reason of the first unhealthy container
	// (e.g. CrashLoopBackOff, ImagePullBackOff); empty when healthy.
	Message string
	// ReadyReplicas is how many pods report ready (from the Deployment status).
	ReadyReplicas int32
	// CPU / Memory are the container's resource requests as k8s quantity
	// strings ("" when unset).
	CPU    string
	Memory string
	// Command is the container argv override (nil = the image default).
	Command []string
	// Env is the container's environment variables.
	Env map[string]string
	// Volumes are the PVCs mounted into the container.
	Volumes []VolumeMount
	// ---- Tier 0 detail (observed from the live Deployment) ----
	ConfigMounts   []ConfigMount
	Resources      Resources
	ReadinessProbe Probe
	LivenessProbe  Probe
	StartupProbe   Probe
	Rollout        Rollout
	SidecarCount   int32
	// Revision is the Deployment's current rollout revision (RollbackService).
	Revision int64
}

// LogOptions selects which container log to read.
type LogOptions struct {
	// TailLines limits the returned lines (0 = all available in the buffer).
	TailLines int64
	// Previous reads the PREVIOUS container instance (the crash that restarted
	// it); useful for CrashLoopBackOff.
	Previous bool
	// Follow keeps streaming new lines until the stream ends (watch mode).
	Follow bool
}

// LogSource reads a service's container logs. It is a seam so a future
// centralized log backend (e.g. Loki) can be added without touching the RPC or
// webui. The default implementation reads the Kubernetes pod log directly.
type LogSource interface {
	// Tail returns up to opts.TailLines lines (Follow=false).
	Tail(ctx context.Context, name string, opts LogOptions) ([]string, error)
	// Follow streams lines until ctx is cancelled or the container exits.
	Follow(ctx context.Context, name string, opts LogOptions) (<-chan string, <-chan error)
}

// Port is one exposed port of a service.
type Port struct {
	// Suffix selects the k8s Service: "" = the primary `<name>`; else the
	// sibling `<name>-<suffix>`. Ports sharing a suffix live in one Service.
	Suffix string
	// Port is the k8s Service port (80 or 443).
	Port int32
	// Protocol is "tcp" or "udp".
	Protocol string
	// TargetPort is the container port to forward to.
	TargetPort int32
}

// PresetPorts maps a preset name to its (port, protocol).
func PresetPorts(preset string) (port int32, proto string, ok bool) {
	switch preset {
	case "tcp80":
		return 80, "tcp", true
	case "tcp443":
		return 443, "tcp", true
	case "udp443":
		return 443, "udp", true
	}
	return 0, "", false
}

// Probe is a container probe. Exactly one of HTTPGet/TCP/Exec should be set;
// HTTPGet wins over TCP over Exec.
type Probe struct {
	HTTPPath            string
	HTTPPort            int32
	TCPPort             int32
	Exec                []string
	InitialDelaySeconds int32
	PeriodSeconds       int32
	TimeoutSeconds      int32
	FailureThreshold    int32
	SuccessThreshold    int32
}

// Empty reports whether the probe has no action (so it should be omitted).
func (p Probe) Empty() bool {
	return p.HTTPPort == 0 && p.TCPPort == 0 && len(p.Exec) == 0
}

// Resources splits requests from limits (each field optional).
type Resources struct {
	CPURequest    string
	MemoryRequest string
	CPULimit      string
	MemoryLimit   string
}

// Empty reports whether no request/limit was given.
func (r Resources) Empty() bool {
	return r.CPURequest == "" && r.MemoryRequest == "" && r.CPULimit == "" && r.MemoryLimit == ""
}

// EnvRef injects one env var from a ConfigMap/Secret key.
type EnvRef struct {
	Name      string
	ConfigMap string
	ConfigKey string
	Secret    string
	SecretKey string
}

// EnvFrom injects all keys of a ConfigMap or Secret as env vars.
type EnvFrom struct {
	ConfigMap string
	Secret    string
}

// ConfigMount mounts a ConfigMap or Secret as a volume.
type ConfigMount struct {
	ConfigMap string
	Secret    string
	MountPath string
	Items     []KeyToPath
}

// KeyToPath selects one key of a ConfigMap/Secret into a relative file path.
type KeyToPath struct {
	Key  string
	Path string
}

// Sidecar is an extra container in the pod (Init=true => an init container).
type Sidecar struct {
	Name    string
	Image   string
	Command []string
	Env     map[string]string
	CPU     string
	Memory  string
	Init    bool
}

// Toleration is one pod toleration.
type Toleration struct {
	Key      string
	Operator string
	Value    string
	Effect   string
}

// Rollout is the Deployment's RollingUpdate strategy.
type Rollout struct {
	MaxSurge       string
	MaxUnavailable string
}

// Spec describes a service to deploy.
type Spec struct {
	Name          string
	Image         string
	Command       []string
	Env           map[string]string
	CPU           string
	Memory        string
	Replicas      int32
	ContainerPort int32
	ServicePort   int32
	Creator       string
	// Session is the session that deployed the service (empty = unbound).
	Session string
	// Namespace is the tenant-internal namespace (org segment of the session).
	Namespace string
	// Ports are the resolved ports to expose. Empty = the default single public
	// port (tcp80 -> ContainerPort). Entries sharing a Suffix form one Service.
	Ports []Port
	// Runtime is the rendered runtime profile (device limits, security context,
	// tun mount, node selector, ...). Zero value = a plain service.
	Runtime runtimeprofiles.Rendered
	// Volumes are named PVCs to mount into the container.
	Volumes []VolumeMount
	// ---- Tier 0 enhancements (all optional) ----
	Resources      Resources
	ReadinessProbe Probe
	LivenessProbe  Probe
	StartupProbe   Probe
	Rollout        Rollout
	EnvRefs        []EnvRef
	EnvFrom        []EnvFrom
	ConfigMounts   []ConfigMount
	Sidecars       []Sidecar
	NodeSelector   map[string]string
	Tolerations    []Toleration
}

// Client wraps the typed clientset plus the target namespace.
type Client struct {
	cs        kubernetes.Interface
	namespace string
	res       serviceResources
	// registryHost, when set, is the deployment registry (artifact). A service
	// image that names Docker Hub (bare name or docker.io/...) is rewritten to
	// pull from it (see MirrorDockerHubImage).
	registryHost string
}

// serviceResources holds the deployment's default service request/limit values.
type serviceResources struct {
	cpuReq, cpuLim, memReq, memLim string
}

// Config configures a Client.
type Config struct {
	Namespace string
	// RegistryHost is the deployment registry (artifact). Docker Hub service
	// images are rewritten to it (pull-through), so a bare `python:3.14-slim`
	// works even though docker.io is unreachable from the cluster. Empty =
	// no rewrite (image used as-is).
	RegistryHost string
	// Default resource REQUESTS/LIMITS applied when a service omits cpu/memory.
	// Requests default to 500m / 1Gi; limits default to 2 / 4Gi. An explicit
	// cpu/memory on the service still sets request == limit.
	CPURequest    string
	CPULimit      string
	MemoryRequest string
	MemoryLimit   string
}

// New builds a client from in-cluster config, or KUBECONFIG for dev.
func New(cfg Config) (*Client, error) {
	rc, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			return nil, fmt.Errorf("servicesmgr: no in-cluster config and KUBECONFIG unset: %w", err)
		}
		rc, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("servicesmgr: kubeconfig: %w", err)
		}
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("servicesmgr: clientset: %w", err)
	}
	return NewWithClientset(cs, cfg), nil
}

// NewWithClientset is the test/DI seam.
func NewWithClientset(cs kubernetes.Interface, cfg Config) *Client {
	ns := cfg.Namespace
	if ns == "" {
		ns = "worker"
	}
	return &Client{
		cs: cs, namespace: ns,
		registryHost: cfg.RegistryHost,
		res: serviceResources{
			cpuReq: cfg.CPURequest, cpuLim: cfg.CPULimit,
			memReq: cfg.MemoryRequest, memLim: cfg.MemoryLimit,
		},
	}
}

// Namespace returns the managed namespace.
func (c *Client) Namespace() string { return c.namespace }

// Deploy creates or updates the Deployment + its Service(s) for a spec
// (idempotent). Ports are grouped by suffix: "" -> the primary Service
// `<name>`; a non-empty suffix -> a sibling Service `<name>-<suffix>` sharing
// the same pod selector.
func (c *Client) Deploy(ctx context.Context, s Spec) (Service, error) {
	if s.Name == "" || s.Image == "" {
		return Service{}, fmt.Errorf("name and image required")
	}
	s.Image = MirrorDockerHubImage(s.Image, c.registryHost)
	for i := range s.Sidecars {
		s.Sidecars[i].Image = MirrorDockerHubImage(s.Sidecars[i].Image, c.registryHost)
	}
	if s.ContainerPort == 0 {
		s.ContainerPort = 8080
	}
	ports := s.Ports
	if len(ports) == 0 {
		// Default: a single public port 80 -> the container port.
		ports = []Port{{Port: 80, Protocol: "tcp", TargetPort: s.ContainerPort}}
	}
	// Validate ports + group by suffix.
	groups := map[string][]Port{}
	order := []string{}
	for i := range ports {
		p := &ports[i]
		if p.Protocol == "" {
			p.Protocol = "tcp"
		}
		if p.Protocol != "tcp" && p.Protocol != "udp" {
			return Service{}, fmt.Errorf("port %d: protocol must be tcp|udp", i)
		}
		if p.Port != 80 && p.Port != 443 {
			return Service{}, fmt.Errorf("port %d: service port must be 80 or 443", i)
		}
		if p.TargetPort == 0 {
			p.TargetPort = s.ContainerPort
		}
		if _, dup := groups[p.Suffix]; !dup {
			order = append(order, p.Suffix)
		}
		groups[p.Suffix] = append(groups[p.Suffix], *p)
	}
	// Conflict precheck: every Service we would write must be either absent or
	// OUR managed object (same tenant). Never clobber a foreign Service.
	for _, suffix := range order {
		svcName := siblingName(s.Name, suffix)
		cur, err := c.cs.CoreV1().Services(c.namespace).Get(ctx, svcName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return Service{}, err
		}
		if cur.Labels[LabelManaged] != "1" || (s.Creator != "" && cur.Annotations[AnnoCreator] != s.Creator) {
			return Service{}, fmt.Errorf("service %q already exists and is not managed by this tenant", svcName)
		}
	}

	reps := s.Replicas
	if reps <= 0 {
		reps = 1
	}
	labels := map[string]string{LabelManaged: "1", LabelName: s.Name, "app": s.Name}
	annotations := map[string]string{
		AnnoImage: s.Image, AnnoCreator: s.Creator, AnnoSession: s.Session, AnnoNamespace: s.Namespace,
	}

	env := []corev1.EnvVar{}
	for k, v := range s.Env {
		env = append(env, corev1.EnvVar{Name: k, Value: v})
	}
	for k, v := range s.Runtime.Env {
		env = append(env, corev1.EnvVar{Name: k, Value: v})
	}
	// Env from ConfigMap/Secret keys, then whole-config envFrom.
	for i, e := range s.EnvRefs {
		if e.Name == "" {
			return Service{}, fmt.Errorf("env_refs[%d]: name is required", i)
		}
		switch {
		case e.ConfigMap != "" && e.ConfigKey != "":
			env = append(env, corev1.EnvVar{Name: e.Name, ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: e.ConfigMap},
					Key:                  e.ConfigKey,
				},
			}})
		case e.Secret != "" && e.SecretKey != "":
			env = append(env, corev1.EnvVar{Name: e.Name, ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: e.Secret},
					Key:                  e.SecretKey,
				},
			}})
		default:
			return Service{}, fmt.Errorf("env_refs[%d]: set (config_map+config_key) or (secret+secret_key)", i)
		}
	}
	envFrom := []corev1.EnvFromSource{}
	for i, e := range s.EnvFrom {
		switch {
		case e.ConfigMap != "":
			envFrom = append(envFrom, corev1.EnvFromSource{ConfigMapRef: &corev1.ConfigMapEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: e.ConfigMap},
			}})
		case e.Secret != "":
			envFrom = append(envFrom, corev1.EnvFromSource{SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: e.Secret},
			}})
		default:
			return Service{}, fmt.Errorf("env_from[%d]: set config_map or secret", i)
		}
	}
	rr := c.resources(s.CPU, s.Memory)
	if !s.Resources.Empty() {
		rr = buildResources(s.Resources)
	}
	if s.Runtime.Resources != nil {
		rr = *s.Runtime.Resources
	}
	for k, v := range s.Runtime.DeviceLimits {
		if rr.Limits == nil {
			rr.Limits = corev1.ResourceList{}
		}
		rr.Limits[corev1.ResourceName(k)] = resource.MustParse(v)
	}
	// Container ports: the distinct target ports across all exposed ports.
	containerPorts := []corev1.ContainerPort{}
	seen := map[int32]bool{}
	for _, p := range ports {
		if seen[p.TargetPort] {
			continue
		}
		seen[p.TargetPort] = true
		containerPorts = append(containerPorts, corev1.ContainerPort{
			Name: fmt.Sprintf("p%d", p.TargetPort), ContainerPort: p.TargetPort,
		})
	}
	container := corev1.Container{
		Name:            "svc",
		Image:           s.Image,
		Env:             env,
		EnvFrom:         envFrom,
		Command:         s.Command,
		Ports:           containerPorts,
		Resources:       rr,
		SecurityContext: s.Runtime.SecurityContext,
	}
	if pr := buildProbe(s.ReadinessProbe); pr != nil {
		container.ReadinessProbe = pr
	}
	if pl := buildProbe(s.LivenessProbe); pl != nil {
		container.LivenessProbe = pl
	}
	if ps := buildProbe(s.StartupProbe); ps != nil {
		container.StartupProbe = ps
	}
	podSpec := corev1.PodSpec{Containers: []corev1.Container{container}}
	// ConfigMap/Secret volumes.
	initContainers := []corev1.Container{}
	extraContainers := []corev1.Container{}
	for i, cm := range s.ConfigMounts {
		if cm.MountPath == "" {
			return Service{}, fmt.Errorf("config_mounts[%d]: mount_path is required", i)
		}
		volName := fmt.Sprintf("cfg-%d", i)
		vol := corev1.Volume{Name: volName}
		switch {
		case cm.ConfigMap != "":
			src := &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: cm.ConfigMap},
			}
			for _, it := range cm.Items {
				src.Items = append(src.Items, corev1.KeyToPath{Key: it.Key, Path: it.Path})
			}
			vol.ConfigMap = src
		case cm.Secret != "":
			src := &corev1.SecretVolumeSource{SecretName: cm.Secret}
			for _, it := range cm.Items {
				src.Items = append(src.Items, corev1.KeyToPath{Key: it.Key, Path: it.Path})
			}
			vol.Secret = src
		default:
			return Service{}, fmt.Errorf("config_mounts[%d]: set config_map or secret", i)
		}
		podSpec.Volumes = append(podSpec.Volumes, vol)
		podSpec.Containers[0].VolumeMounts = append(podSpec.Containers[0].VolumeMounts, corev1.VolumeMount{
			Name: volName, MountPath: cm.MountPath,
		})
	}
	// Extra containers (init or sidecar).
	for i, sc := range s.Sidecars {
		if sc.Name == "" || sc.Image == "" {
			return Service{}, fmt.Errorf("sidecars[%d]: name and image are required", i)
		}
		c := corev1.Container{
			Name:      sc.Name,
			Image:     sc.Image,
			Command:   sc.Command,
			Resources: c.resources(sc.CPU, sc.Memory),
		}
		for k, v := range sc.Env {
			c.Env = append(c.Env, corev1.EnvVar{Name: k, Value: v})
		}
		if sc.Init {
			initContainers = append(initContainers, c)
		} else {
			extraContainers = append(extraContainers, c)
		}
	}
	if len(initContainers) > 0 {
		podSpec.InitContainers = initContainers
	}
	if len(extraContainers) > 0 {
		podSpec.Containers = append(podSpec.Containers, extraContainers...)
	}
	// Tolerations (node selector merges with the runtime profile's below).
	if len(s.Tolerations) > 0 {
		for i, t := range s.Tolerations {
			if t.Key == "" {
				return Service{}, fmt.Errorf("tolerations[%d]: key is required", i)
			}
			podSpec.Tolerations = append(podSpec.Tolerations, corev1.Toleration{
				Key: t.Key, Operator: corev1.TolerationOperator(t.Operator),
				Value: t.Value, Effect: corev1.TaintEffect(t.Effect),
			})
		}
	}
	if len(s.NodeSelector) > 0 {
		if podSpec.NodeSelector == nil {
			podSpec.NodeSelector = map[string]string{}
		}
		for k, v := range s.NodeSelector {
			podSpec.NodeSelector[k] = v
		}
	}
	if s.Runtime.RuntimeClass != "" {
		rc := s.Runtime.RuntimeClass
		podSpec.RuntimeClassName = &rc
	}
	if len(s.Runtime.NodeSelector) > 0 {
		podSpec.NodeSelector = s.Runtime.NodeSelector
	}
	for _, sec := range s.Runtime.ImagePullSecrets {
		podSpec.ImagePullSecrets = append(podSpec.ImagePullSecrets, corev1.LocalObjectReference{Name: sec})
	}
	if s.Runtime.NeedsTun {
		t := corev1.HostPathCharDev
		podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
			Name: "devtun",
			VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
				Path: "/dev/net/tun", Type: &t,
			}},
		})
		podSpec.Containers[0].VolumeMounts = append(podSpec.Containers[0].VolumeMounts, corev1.VolumeMount{
			Name: "devtun", MountPath: "/dev/net/tun",
		})
	}
	// Named PVC mounts. A claim is referenced by name in the managed namespace.
	for i, vm := range s.Volumes {
		if vm.PVC == "" || vm.MountPath == "" {
			return Service{}, fmt.Errorf("volumes[%d]: pvc and mount_path are required", i)
		}
		volName := fmt.Sprintf("pvc-%d", i)
		podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
			Name: volName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: vm.PVC,
					ReadOnly:  vm.ReadOnly,
				},
			},
		})
		podSpec.Containers[0].VolumeMounts = append(podSpec.Containers[0].VolumeMounts, corev1.VolumeMount{
			Name: volName, MountPath: vm.MountPath, ReadOnly: vm.ReadOnly, SubPath: vm.SubPath,
		})
	}
	depSpec := appsv1.DeploymentSpec{
		Replicas: &reps,
		Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": s.Name}},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: annotations},
			Spec:       podSpec,
		},
	}
	if ru := buildRollout(s.Rollout); ru != nil {
		depSpec.Strategy = appsv1.DeploymentStrategy{
			Type: appsv1.RollingUpdateDeploymentStrategyType, RollingUpdate: ru,
		}
	}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: s.Name, Namespace: c.namespace, Labels: labels, Annotations: annotations},
		Spec:       depSpec,
	}
	if err := c.applyDeployment(ctx, dep); err != nil {
		return Service{}, err
	}

	// One Service per suffix, each selecting the same pod.
	for _, suffix := range order {
		svcName := siblingName(s.Name, suffix)
		svcLabels := map[string]string{LabelManaged: "1", LabelName: s.Name, "app": s.Name}
		svcAnnotations := map[string]string{
			AnnoImage: s.Image, AnnoCreator: s.Creator, AnnoSession: s.Session, AnnoNamespace: s.Namespace,
			AnnoPrimary: s.Name,
		}
		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: svcName, Namespace: c.namespace, Labels: svcLabels, Annotations: svcAnnotations},
			Spec: corev1.ServiceSpec{
				Selector: map[string]string{"app": s.Name},
				Ports:    corePorts(groups[suffix]),
			},
		}
		if err := c.applyService(ctx, svc); err != nil {
			return Service{}, err
		}
	}
	return c.Get(ctx, s.Name)
}

// siblingName is the k8s Service name for a port group: the primary name for
// the "" suffix, else `<name>-<suffix>`.
func siblingName(name, suffix string) string {
	if suffix == "" {
		return name
	}
	return name + "-" + suffix
}

// corePorts converts our Port list to k8s ServicePorts (names are unique per
// Service, so tcp443/udp443 can coexist).
func corePorts(ports []Port) []corev1.ServicePort {
	out := make([]corev1.ServicePort, 0, len(ports))
	for _, p := range ports {
		proto := corev1.ProtocolTCP
		if p.Protocol == "udp" {
			proto = corev1.ProtocolUDP
		}
		out = append(out, corev1.ServicePort{
			Name:       fmt.Sprintf("%s-%d", p.Protocol, p.Port),
			Port:       p.Port,
			Protocol:   proto,
			TargetPort: intstr.FromInt32(p.TargetPort),
		})
	}
	return out
}

// AnnoPrimary marks a sibling Service with the primary service it belongs to.
const AnnoPrimary = "workspace/service-primary"

func (c *Client) applyDeployment(ctx context.Context, dep *appsv1.Deployment) error {
	_, err := c.cs.AppsV1().Deployments(c.namespace).Create(ctx, dep, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		cur, gerr := c.cs.AppsV1().Deployments(c.namespace).Get(ctx, dep.Name, metav1.GetOptions{})
		if gerr != nil {
			return gerr
		}
		dep.ResourceVersion = cur.ResourceVersion
		_, err = c.cs.AppsV1().Deployments(c.namespace).Update(ctx, dep, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("apply deployment: %w", err)
	}
	return nil
}

func (c *Client) applyService(ctx context.Context, svc *corev1.Service) error {
	_, err := c.cs.CoreV1().Services(c.namespace).Create(ctx, svc, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		cur, gerr := c.cs.CoreV1().Services(c.namespace).Get(ctx, svc.Name, metav1.GetOptions{})
		if gerr != nil {
			return gerr
		}
		svc.ResourceVersion = cur.ResourceVersion
		svc.Spec.ClusterIP = cur.Spec.ClusterIP
		_, err = c.cs.CoreV1().Services(c.namespace).Update(ctx, svc, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("apply service: %w", err)
	}
	return nil
}

// Get returns one service by name.
func (c *Client) Get(ctx context.Context, name string) (Service, error) {
	d, err := c.cs.AppsV1().Deployments(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Service{}, err
	}
	svc := toService(c, d, c.portsFor(ctx, name))
	svc.PodPhase, svc.Restarts, svc.Message = c.podDiagnostics(ctx, name)
	return svc, nil
}

// portsFor collects every port across the primary Service and its siblings.
func (c *Client) portsFor(ctx context.Context, name string) []Port {
	list, err := c.cs.CoreV1().Services(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: LabelManaged + "=1," + LabelName + "=" + name})
	if err != nil {
		return nil
	}
	out := []Port{}
	for i := range list.Items {
		out = append(out, portsFromService(&list.Items[i], name)...)
	}
	return out
}

// portsFromService maps one k8s Service to its ports; `name` is the logical
// service name (so the suffix is derived from a sibling `<name>-<suffix>`).
func portsFromService(svc *corev1.Service, name string) []Port {
	out := []Port{}
	suffix := ""
	if svc.Name != name {
		suffix = strings.TrimPrefix(svc.Name, name+"-")
	}
	for _, p := range svc.Spec.Ports {
		proto := "tcp"
		if p.Protocol == corev1.ProtocolUDP {
			proto = "udp"
		}
		out = append(out, Port{
			Suffix: suffix, Port: p.Port, Protocol: proto,
			TargetPort: p.TargetPort.IntVal,
		})
	}
	return out
}

// Exists reports whether a managed service exists.
func (c *Client) Exists(ctx context.Context, name string) (bool, error) {
	_, err := c.cs.AppsV1().Deployments(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// List returns every managed service.
func (c *Client) List(ctx context.Context) ([]Service, error) {
	list, err := c.cs.AppsV1().Deployments(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: LabelManaged + "=1"})
	if err != nil {
		return nil, err
	}
	portsBy := c.portsByService(ctx)
	diagBy := c.podDiagnosticsByService(ctx)
	out := make([]Service, 0, len(list.Items))
	for i := range list.Items {
		d := &list.Items[i]
		svc := toService(c, d, portsBy[d.Name])
		svc.PodPhase, svc.Restarts, svc.Message = diagBy[d.Name].phase, diagBy[d.Name].restarts, diagBy[d.Name].message
		out = append(out, svc)
	}
	return out, nil
}

// ---- persistent volume claims ----

// CreatePVC creates a named claim in the managed namespace with the given
// storage class (idempotent: an existing claim of the same name is returned
// unchanged, and its creator must match).
func (c *Client) CreatePVC(ctx context.Context, name, size, storageClass, creator, namespace string) (PVC, error) {
	if name == "" {
		return PVC{}, fmt.Errorf("name required")
	}
	if size == "" {
		size = "1Gi"
	}
	q, err := resource.ParseQuantity(size)
	if err != nil {
		return PVC{}, fmt.Errorf("invalid size %q: %w", size, err)
	}
	if existing, gerr := c.GetPVC(ctx, name); gerr == nil {
		if existing.Creator != "" && creator != "" && existing.Creator != creator {
			return PVC{}, fmt.Errorf("pvc %q belongs to another tenant", name)
		}
		return existing, nil
	} else if !apierrors.IsNotFound(gerr) {
		return PVC{}, gerr
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   c.namespace,
			Labels:      map[string]string{LabelPVC: "1", LabelPVCName: name},
			Annotations: map[string]string{AnnoPVCCreator: creator, AnnoPVCNamespace: namespace},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: q}},
		},
	}
	if storageClass != "" {
		sc := storageClass
		pvc.Spec.StorageClassName = &sc
	}
	if _, err := c.cs.CoreV1().PersistentVolumeClaims(c.namespace).Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
		return PVC{}, fmt.Errorf("create pvc: %w", err)
	}
	return c.GetPVC(ctx, name)
}

// GetPVC returns one claim by name.
func (c *Client) GetPVC(ctx context.Context, name string) (PVC, error) {
	p, err := c.cs.CoreV1().PersistentVolumeClaims(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return PVC{}, err
	}
	return toPVC(p, c.mountedBy(ctx, name)), nil
}

// ListPVCs returns every managed claim.
func (c *Client) ListPVCs(ctx context.Context) ([]PVC, error) {
	list, err := c.cs.CoreV1().PersistentVolumeClaims(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: LabelPVC + "=1"})
	if err != nil {
		return nil, err
	}
	mounted := c.mountedIndex(ctx)
	out := make([]PVC, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, toPVC(&list.Items[i], mounted[list.Items[i].Name]))
	}
	return out, nil
}

// DeletePVC removes a claim. It REFUSES when any managed Deployment still mounts
// it, so a running service never loses its volume under it.
func (c *Client) DeletePVC(ctx context.Context, name string) (bool, error) {
	mounted := c.mountedBy(ctx, name)
	if len(mounted) > 0 {
		return false, fmt.Errorf("pvc %q is mounted by: %s", name, strings.Join(mounted, ", "))
	}
	err := c.cs.CoreV1().PersistentVolumeClaims(c.namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// mountedBy returns the names of managed Deployments that mount the claim.
func (c *Client) mountedBy(ctx context.Context, name string) []string {
	return c.mountedIndex(ctx)[name]
}

// mountedIndex maps each claim name to the managed Deployments that mount it,
// from ONE Deployments List (so a PVC list is O(1) k8s calls, not O(N)).
func (c *Client) mountedIndex(ctx context.Context) map[string][]string {
	out := map[string][]string{}
	deps, err := c.cs.AppsV1().Deployments(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: LabelManaged + "=1"})
	if err != nil {
		return out
	}
	for i := range deps.Items {
		for _, v := range deps.Items[i].Spec.Template.Spec.Volumes {
			if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName != "" {
				out[v.PersistentVolumeClaim.ClaimName] = append(out[v.PersistentVolumeClaim.ClaimName], deps.Items[i].Name)
			}
		}
	}
	return out
}

func toPVC(p *corev1.PersistentVolumeClaim, mountedBy []string) PVC {
	size := ""
	if q, ok := p.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
		size = q.String()
	}
	sc := ""
	if p.Spec.StorageClassName != nil {
		sc = *p.Spec.StorageClassName
	}
	return PVC{
		Name: p.Name, Size: size, StorageClass: sc, Phase: string(p.Status.Phase),
		Creator: p.Annotations[AnnoPVCCreator], Namespace: p.Annotations[AnnoPVCNamespace],
		CreatedAt: p.CreationTimestamp.UnixMilli(),
		MountedBy: mountedBy,
	}
}

// Pause scales a service's Deployment to zero replicas, remembering the count
// it had (so Resume restores it). Idempotent: pausing an already-paused service
// keeps the originally remembered count. Returns the updated view.
func (c *Client) Pause(ctx context.Context, name string) (Service, error) {
	d, err := c.cs.AppsV1().Deployments(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Service{}, err
	}
	if _, paused := d.Annotations[AnnoReplicasBeforePause]; !paused {
		prev := int32(0)
		if d.Spec.Replicas != nil {
			prev = *d.Spec.Replicas
		}
		if prev < 1 {
			prev = 1
		}
		if d.Annotations == nil {
			d.Annotations = map[string]string{}
		}
		d.Annotations[AnnoReplicasBeforePause] = fmt.Sprint(prev)
	}
	zero := int32(0)
	d.Spec.Replicas = &zero
	if _, err := c.cs.AppsV1().Deployments(c.namespace).Update(ctx, d, metav1.UpdateOptions{}); err != nil {
		return Service{}, err
	}
	return c.Get(ctx, name)
}

// Resume restores a paused service to the replica count remembered at pause
// time (default 1 when absent) and clears the pause marker. Returns the
// updated view.
func (c *Client) Resume(ctx context.Context, name string) (Service, error) {
	d, err := c.cs.AppsV1().Deployments(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Service{}, err
	}
	reps := int32(1)
	if v := d.Annotations[AnnoReplicasBeforePause]; v != "" {
		if n, perr := strconv.ParseInt(v, 10, 32); perr == nil && n > 0 {
			reps = int32(n)
		}
	}
	d.Spec.Replicas = &reps
	delete(d.Annotations, AnnoReplicasBeforePause)
	if _, err := c.cs.AppsV1().Deployments(c.namespace).Update(ctx, d, metav1.UpdateOptions{}); err != nil {
		return Service{}, err
	}
	return c.Get(ctx, name)
}

// Scale sets a service's desired replica count (0 = scaled down). Unlike Pause
// it does not remember the previous count, and it clears any pause marker.
func (c *Client) Scale(ctx context.Context, name string, replicas int32) (Service, error) {
	if replicas < 0 {
		return Service{}, fmt.Errorf("replicas must be >= 0")
	}
	d, err := c.cs.AppsV1().Deployments(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Service{}, err
	}
	if d.Annotations != nil {
		delete(d.Annotations, AnnoReplicasBeforePause)
	}
	d.Spec.Replicas = &replicas
	if _, err := c.cs.AppsV1().Deployments(c.namespace).Update(ctx, d, metav1.UpdateOptions{}); err != nil {
		return Service{}, err
	}
	return c.Get(ctx, name)
}

// Manifest returns the service's Deployment + every bound Service as a
// multi-document YAML (`---`-separated, Deployment first). Server-managed
// metadata (status, uid, resourceVersion, creationTimestamp, managedFields) is
// stripped so the result is a clean, re-appliable spec.
func (c *Client) Manifest(ctx context.Context, name string) (string, error) {
	d, err := c.cs.AppsV1().Deployments(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	docs := []any{cleanDeployment(d)}
	svcs, lerr := c.cs.CoreV1().Services(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: LabelManaged + "=1," + LabelName + "=" + name})
	if lerr == nil {
		// Primary first, then siblings by name.
		sort.SliceStable(svcs.Items, func(i, j int) bool { return svcs.Items[i].Name < svcs.Items[j].Name })
		for i := range svcs.Items {
			docs = append(docs, cleanService(&svcs.Items[i]))
		}
	}
	var b strings.Builder
	for i, doc := range docs {
		if i > 0 {
			b.WriteString("---\n")
		}
		y, merr := yaml.Marshal(doc)
		if merr != nil {
			return "", merr
		}
		b.Write(y)
	}
	return b.String(), nil
}

// cleanDeployment deep-copies a Deployment with server-managed fields cleared,
// so it round-trips through YAML/apply cleanly.
func cleanDeployment(d *appsv1.Deployment) *appsv1.Deployment {
	out := d.DeepCopy()
	out.TypeMeta = metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}
	out.Status = appsv1.DeploymentStatus{}
	out.ResourceVersion = ""
	out.UID = ""
	out.CreationTimestamp = metav1.Time{}
	out.ManagedFields = nil
	out.Generation = 0
	out.SelfLink = ""
	// Drop server-managed annotations (rollout revision, restart marker) so the
	// YAML round-trips cleanly.
	delete(out.Annotations, "deployment.kubernetes.io/revision")
	delete(out.Annotations, "kubectl.kubernetes.io/restartedAt")
	if out.Spec.Template.ObjectMeta.Annotations != nil {
		delete(out.Spec.Template.ObjectMeta.Annotations, "kubectl.kubernetes.io/restartedAt")
	}
	return out
}

func cleanService(s *corev1.Service) *corev1.Service {
	out := s.DeepCopy()
	out.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Service"}
	out.ResourceVersion = ""
	out.UID = ""
	out.CreationTimestamp = metav1.Time{}
	out.ManagedFields = nil
	out.Generation = 0
	out.SelfLink = ""
	// Server-assigned networking fields.
	out.Spec.ClusterIP = ""
	out.Spec.ClusterIPs = nil
	out.Spec.IPFamilies = nil
	out.Spec.IPFamilyPolicy = nil
	out.Spec.HealthCheckNodePort = 0
	for i := range out.Spec.Ports {
		out.Spec.Ports[i].NodePort = 0
	}
	out.Status = corev1.ServiceStatus{}
	return out
}

// ApplyManifest parses a multi-document YAML (a Deployment + its Services) and
// applies it to the EXISTING service `name`. The Deployment's name, namespace,
// selector and managed labels are pinned so the manifest cannot retarget
// another service. Service objects are accepted only when their
// `workspace/service-name` label equals `name`. `dryRun` validates + returns
// the normalized YAML without writing.
func (c *Client) ApplyManifest(ctx context.Context, name, manifest string, dryRun bool) (Service, string, error) {
	cur, err := c.cs.AppsV1().Deployments(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Service{}, "", err
	}
	var dep *appsv1.Deployment
	var svcs []*corev1.Service
	for _, doc := range splitYAMLDocs(manifest) {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var probe struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal([]byte(doc), &probe); err != nil {
			return Service{}, "", fmt.Errorf("parse yaml: %w", err)
		}
		switch strings.ToLower(probe.Kind) {
		case "deployment":
			var d appsv1.Deployment
			if err := yaml.Unmarshal([]byte(doc), &d); err != nil {
				return Service{}, "", fmt.Errorf("parse deployment: %w", err)
			}
			dep = &d
		case "service":
			var s corev1.Service
			if err := yaml.Unmarshal([]byte(doc), &s); err != nil {
				return Service{}, "", fmt.Errorf("parse service: %w", err)
			}
			svcs = append(svcs, &s)
		default:
			return Service{}, "", fmt.Errorf("unsupported kind %q (only Deployment + Service)", probe.Kind)
		}
	}
	if dep == nil {
		return Service{}, "", fmt.Errorf("manifest has no Deployment")
	}
	// Pin identity: name/namespace/selector/labels must match the existing
	// managed service so a manifest cannot retarget another Deployment.
	if dep.Name != "" && dep.Name != name {
		return Service{}, "", fmt.Errorf("deployment name %q does not match %q", dep.Name, name)
	}
	dep.Name = name
	dep.Namespace = c.namespace
	dep.Labels = map[string]string{LabelManaged: "1", LabelName: name, "app": name}
	dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}}
	dep.Spec.Template.ObjectMeta.Labels = map[string]string{LabelManaged: "1", LabelName: name, "app": name}
	// Preserve managed annotations (creator/session/stage/expiry) from the
	// current object; keep any user annotations.
	dep.Annotations = mergeAnnotations(cur.Annotations, dep.Annotations)
	for i := range svcs {
		s := svcs[i]
		if s.Labels[LabelName] != "" && s.Labels[LabelName] != name {
			return Service{}, "", fmt.Errorf("service %q belongs to another service", s.Name)
		}
		s.Namespace = c.namespace
		if s.Labels == nil {
			s.Labels = map[string]string{}
		}
		s.Labels[LabelManaged] = "1"
		s.Labels[LabelName] = name
		s.Labels["app"] = name
		s.Spec.Selector = map[string]string{"app": name}
		s.Spec.ClusterIP = ""
		s.Spec.ClusterIPs = nil
	}
	// Build the normalized YAML for the response.
	normalized, err := manifestYAML(dep, svcs)
	if err != nil {
		return Service{}, "", err
	}
	if dryRun {
		return Service{}, normalized, nil
	}
	// Apply the Deployment, then each Service (create-or-update).
	if err := c.applyDeployment(ctx, dep); err != nil {
		return Service{}, "", err
	}
	for i := range svcs {
		if err := c.applyService(ctx, svcs[i]); err != nil {
			return Service{}, "", err
		}
	}
	out, err := c.Get(ctx, name)
	if err != nil {
		return Service{}, "", err
	}
	return out, normalized, nil
}

// mergeAnnotations keeps `base` (server-managed) values unless `over` overrides
// them, and never lets an apply drop a managed annotation.
func mergeAnnotations(base, over map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range over {
		out[k] = v
	}
	for _, k := range []string{AnnoImage, AnnoCreator, AnnoSession} {
		if v, ok := base[k]; ok {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func manifestYAML(dep *appsv1.Deployment, svcs []*corev1.Service) (string, error) {
	var b strings.Builder
	y, err := yaml.Marshal(dep)
	if err != nil {
		return "", err
	}
	b.Write(y)
	for _, s := range svcs {
		b.WriteString("---\n")
		y, err := yaml.Marshal(s)
		if err != nil {
			return "", err
		}
		b.Write(y)
	}
	return b.String(), nil
}

// splitYAMLDocs splits a multi-document YAML on lines that are exactly `---`.
func splitYAMLDocs(manifest string) []string {
	var docs []string
	var cur []string
	for _, line := range strings.Split(manifest, "\n") {
		if strings.TrimSpace(line) == "---" {
			docs = append(docs, strings.Join(cur, "\n"))
			cur = nil
			continue
		}
		cur = append(cur, line)
	}
	docs = append(docs, strings.Join(cur, "\n"))
	return docs
}

// Delete removes the Deployment + its Service(s) (primary + siblings).
func (c *Client) Delete(ctx context.Context, name string) (bool, error) {
	_, err := c.cs.AppsV1().Deployments(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	// Delete every Service bound to this deployment (primary + siblings).
	svcs, lerr := c.cs.CoreV1().Services(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: LabelManaged + "=1," + LabelName + "=" + name})
	if lerr == nil {
		for i := range svcs.Items {
			_ = c.cs.CoreV1().Services(c.namespace).Delete(ctx, svcs.Items[i].Name, metav1.DeleteOptions{})
		}
	}
	if err := c.cs.AppsV1().Deployments(c.namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	return true, nil
}

// resources builds a service container's resources. An explicit cpu/memory sets
// request == limit to that value; when omitted, the deployment defaults apply
// (requests 500m/1Gi, limits 2/4Gi => Burstable).
func (c *Client) resources(cpu, mem string) corev1.ResourceRequirements {
	req := corev1.ResourceList{}
	lim := corev1.ResourceList{}
	if cpu != "" {
		q := resource.MustParse(cpu)
		req[corev1.ResourceCPU] = q
		lim[corev1.ResourceCPU] = q
	} else {
		req[corev1.ResourceCPU] = resource.MustParse(svcOrDefault(c.res.cpuReq, "500m"))
		lim[corev1.ResourceCPU] = resource.MustParse(svcOrDefault(c.res.cpuLim, "2"))
	}
	if mem != "" {
		q := resource.MustParse(mem)
		req[corev1.ResourceMemory] = q
		lim[corev1.ResourceMemory] = q
	} else {
		req[corev1.ResourceMemory] = resource.MustParse(svcOrDefault(c.res.memReq, "1Gi"))
		lim[corev1.ResourceMemory] = resource.MustParse(svcOrDefault(c.res.memLim, "4Gi"))
	}
	return corev1.ResourceRequirements{Requests: req, Limits: lim}
}

func svcOrDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// buildResources maps a split request/limit spec to k8s requirements.
func buildResources(r Resources) corev1.ResourceRequirements {
	out := corev1.ResourceRequirements{}
	req := corev1.ResourceList{}
	if r.CPURequest != "" {
		req[corev1.ResourceCPU] = resource.MustParse(r.CPURequest)
	}
	if r.MemoryRequest != "" {
		req[corev1.ResourceMemory] = resource.MustParse(r.MemoryRequest)
	}
	if len(req) > 0 {
		out.Requests = req
	}
	lim := corev1.ResourceList{}
	if r.CPULimit != "" {
		lim[corev1.ResourceCPU] = resource.MustParse(r.CPULimit)
	}
	if r.MemoryLimit != "" {
		lim[corev1.ResourceMemory] = resource.MustParse(r.MemoryLimit)
	}
	if len(lim) > 0 {
		out.Limits = lim
	}
	return out
}

// buildProbe maps a Probe spec to a k8s Probe (nil when no action is set).
func buildProbe(p Probe) *corev1.Probe {
	if p.Empty() {
		return nil
	}
	handler := corev1.ProbeHandler{}
	switch {
	case p.HTTPPort > 0:
		path := p.HTTPPath
		if path == "" {
			path = "/"
		}
		handler.HTTPGet = &corev1.HTTPGetAction{
			Path: path, Port: intstr.FromInt32(p.HTTPPort),
		}
	case p.TCPPort > 0:
		handler.TCPSocket = &corev1.TCPSocketAction{Port: intstr.FromInt32(p.TCPPort)}
	case len(p.Exec) > 0:
		handler.Exec = &corev1.ExecAction{Command: p.Exec}
	}
	probe := &corev1.Probe{ProbeHandler: handler}
	if p.InitialDelaySeconds > 0 {
		probe.InitialDelaySeconds = p.InitialDelaySeconds
	}
	if p.PeriodSeconds > 0 {
		probe.PeriodSeconds = p.PeriodSeconds
	}
	if p.TimeoutSeconds > 0 {
		probe.TimeoutSeconds = p.TimeoutSeconds
	}
	if p.FailureThreshold > 0 {
		probe.FailureThreshold = p.FailureThreshold
	}
	if p.SuccessThreshold > 0 {
		probe.SuccessThreshold = p.SuccessThreshold
	}
	return probe
}

// buildRollout maps a Rollout spec to k8s int-or-percent values (nil when
// neither is set).
func buildRollout(r Rollout) *appsv1.RollingUpdateDeployment {
	if r.MaxSurge == "" && r.MaxUnavailable == "" {
		return nil
	}
	out := &appsv1.RollingUpdateDeployment{}
	if r.MaxSurge != "" {
		v := intstr.Parse(r.MaxSurge)
		out.MaxSurge = &v
	}
	if r.MaxUnavailable != "" {
		v := intstr.Parse(r.MaxUnavailable)
		out.MaxUnavailable = &v
	}
	return out
}

func toService(c *Client, d *appsv1.Deployment, ports []Port) Service {
	phase := "Pending"
	if d.Status.ReadyReplicas > 0 {
		phase = "Running"
	} else if d.Status.UnavailableReplicas > 0 {
		phase = "Progressing"
	}
	reps := int32(0)
	if d.Spec.Replicas != nil {
		reps = *d.Spec.Replicas
	}
	host := fmt.Sprintf("%s.%s.svc.cluster.local", d.Name, c.namespace)
	// The in-cluster URL points at the primary Service's first TCP port.
	urlPort := int32(80)
	for _, p := range ports {
		if p.Suffix == "" && p.Protocol == "tcp" {
			urlPort = p.Port
			break
		}
	}
	_, paused := d.Annotations[AnnoReplicasBeforePause]
	svc := Service{
		Name: d.Name, Image: d.Annotations[AnnoImage], Phase: phase,
		Ready: d.Status.ReadyReplicas > 0, Replicas: reps,
		ReadyReplicas: d.Status.ReadyReplicas,
		URL:           fmt.Sprintf("http://%s:%d", host, urlPort),
		Creator:       d.Annotations[AnnoCreator], Session: d.Annotations[AnnoSession],
		CreatedAt: d.CreationTimestamp.UnixMilli(),
		Ports:     ports,
		Paused:    paused,
	}
	// Container detail (first container only — every managed service has one).
	if len(d.Spec.Template.Spec.Containers) > 0 {
		ctr := d.Spec.Template.Spec.Containers[0]
		svc.Command = ctr.Command
		if len(ctr.Env) > 0 {
			svc.Env = map[string]string{}
			for _, e := range ctr.Env {
				svc.Env[e.Name] = e.Value
			}
		}
		if r, ok := ctr.Resources.Requests[corev1.ResourceCPU]; ok {
			svc.CPU = r.String()
		}
		if r, ok := ctr.Resources.Requests[corev1.ResourceMemory]; ok {
			svc.Memory = r.String()
		}
		svc.Resources = Resources{
			CPURequest:    quantityString(ctr.Resources.Requests, corev1.ResourceCPU),
			MemoryRequest: quantityString(ctr.Resources.Requests, corev1.ResourceMemory),
			CPULimit:      quantityString(ctr.Resources.Limits, corev1.ResourceCPU),
			MemoryLimit:   quantityString(ctr.Resources.Limits, corev1.ResourceMemory),
		}
		svc.ReadinessProbe = toProbe(ctr.ReadinessProbe)
		svc.LivenessProbe = toProbe(ctr.LivenessProbe)
		svc.StartupProbe = toProbe(ctr.StartupProbe)
		// Map pod volume names back to PVC / ConfigMap / Secret sources.
		volByPVC := map[string]string{}
		cmByVol := map[string]corev1.Volume{}
		for _, v := range d.Spec.Template.Spec.Volumes {
			cmByVol[v.Name] = v
			if v.PersistentVolumeClaim != nil {
				volByPVC[v.Name] = v.PersistentVolumeClaim.ClaimName
			}
		}
		for _, m := range ctr.VolumeMounts {
			if pvc, ok := volByPVC[m.Name]; ok {
				svc.Volumes = append(svc.Volumes, VolumeMount{
					PVC: pvc, MountPath: m.MountPath, ReadOnly: m.ReadOnly, SubPath: m.SubPath,
				})
				continue
			}
			if v, ok := cmByVol[m.Name]; ok {
				switch {
				case v.ConfigMap != nil:
					svc.ConfigMounts = append(svc.ConfigMounts, ConfigMount{ConfigMap: v.ConfigMap.Name, MountPath: m.MountPath})
				case v.Secret != nil:
					svc.ConfigMounts = append(svc.ConfigMounts, ConfigMount{Secret: v.Secret.SecretName, MountPath: m.MountPath})
				}
			}
		}
		// Extra containers (beyond the first) count as sidecars/init.
		svc.SidecarCount = int32(len(d.Spec.Template.Spec.Containers) - 1 + len(d.Spec.Template.Spec.InitContainers))
	}
	if ru := d.Spec.Strategy.RollingUpdate; ru != nil {
		if ru.MaxSurge != nil {
			svc.Rollout.MaxSurge = ru.MaxSurge.String()
		}
		if ru.MaxUnavailable != nil {
			svc.Rollout.MaxUnavailable = ru.MaxUnavailable.String()
		}
	}
	return svc
}

// quantityString renders a resource quantity ("" when absent).
func quantityString(rl corev1.ResourceList, name corev1.ResourceName) string {
	if q, ok := rl[name]; ok {
		return q.String()
	}
	return ""
}

// toProbe maps a k8s Probe back to our Probe spec.
func toProbe(p *corev1.Probe) Probe {
	if p == nil {
		return Probe{}
	}
	out := Probe{
		InitialDelaySeconds: p.InitialDelaySeconds,
		PeriodSeconds:       p.PeriodSeconds,
		TimeoutSeconds:      p.TimeoutSeconds,
		FailureThreshold:    p.FailureThreshold,
		SuccessThreshold:    p.SuccessThreshold,
	}
	switch {
	case p.HTTPGet != nil:
		out.HTTPPath = p.HTTPGet.Path
		out.HTTPPort = p.HTTPGet.Port.IntVal
	case p.TCPSocket != nil:
		out.TCPPort = p.TCPSocket.Port.IntVal
	case p.Exec != nil:
		out.Exec = p.Exec.Command
	}
	return out
}

// podDiagnostics returns the pod phase, total restarts and the first
// unhealthy container reason for a service (best-effort; zero values when no
// pod is observed). A crashlooping container reports its waiting reason so a
// failure is visible in the service list.
func (c *Client) podDiagnostics(ctx context.Context, name string) (phase string, restarts int32, message string) {
	pods, err := c.cs.CoreV1().Pods(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=" + name})
	if err != nil || len(pods.Items) == 0 {
		return "", 0, ""
	}
	// Prefer the newest pod.
	pod := pods.Items[0]
	for i := range pods.Items {
		if pods.Items[i].CreationTimestamp.After(pod.CreationTimestamp.Time) {
			pod = pods.Items[i]
		}
	}
	phase = string(pod.Status.Phase)
	for _, cs := range pod.Status.ContainerStatuses {
		restarts += cs.RestartCount
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" && message == "" {
			message = cs.State.Waiting.Reason
			if cs.State.Waiting.Message != "" {
				message += ": " + cs.State.Waiting.Message
			}
		}
		if cs.State.Terminated != nil && cs.State.Terminated.Reason != "" && cs.State.Terminated.Reason != "Completed" && message == "" {
			message = cs.State.Terminated.Reason
		}
	}
	return phase, restarts, message
}

// ServiceName derives a k8s-safe service name from a session (org:repo:branch).
func ServiceName(session string) string {
	slug := strings.NewReplacer(":", "-", "/", "-", "_", "-", ".", "-").Replace(strings.ToLower(session))
	slug = strings.Trim(slug, "-")
	if len(slug) > 40 {
		slug = slug[:40]
	}
	if slug == "" {
		return "app"
	}
	return slug
}

// WatchDeployments returns a watch over every managed service Deployment.
func (c *Client) WatchDeployments(ctx context.Context) (watch.Interface, error) {
	return c.cs.AppsV1().Deployments(c.namespace).Watch(ctx, metav1.ListOptions{
		LabelSelector: LabelManaged + "=1",
	})
}

// WatchPVCs returns a watch over every managed PersistentVolumeClaim.
func (c *Client) WatchPVCs(ctx context.Context) (watch.Interface, error) {
	return c.cs.CoreV1().PersistentVolumeClaims(c.namespace).Watch(ctx, metav1.ListOptions{
		LabelSelector: LabelPVC + "=1",
	})
}

// diag is the per-service pod diagnostics summary.
type diag struct {
	phase    string
	restarts int32
	message  string
}

// portsByService maps each managed service name to its exposed ports, in ONE
// Services List (instead of a List per service).
func (c *Client) portsByService(ctx context.Context) map[string][]Port {
	out := map[string][]Port{}
	list, err := c.cs.CoreV1().Services(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: LabelManaged + "=1"})
	if err != nil {
		return out
	}
	for i := range list.Items {
		svc := &list.Items[i]
		name := svc.Labels[LabelName]
		if name == "" {
			name = svc.Name
		}
		out[name] = append(out[name], portsFromService(svc, name)...)
	}
	return out
}

// podDiagnosticsByService maps each managed service name to its newest pod's
// diagnostics, in ONE Pods List (instead of a List per service).
func (c *Client) podDiagnosticsByService(ctx context.Context) map[string]diag {
	out := map[string]diag{}
	pods, err := c.cs.CoreV1().Pods(c.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return out
	}
	newest := map[string]*corev1.Pod{}
	for i := range pods.Items {
		p := &pods.Items[i]
		name := p.Labels["app"]
		if name == "" {
			continue
		}
		if cur := newest[name]; cur == nil || p.CreationTimestamp.After(cur.CreationTimestamp.Time) {
			newest[name] = p
		}
	}
	for name, p := range newest {
		out[name] = diagFromPod(p)
	}
	return out
}

// diagFromPod summarizes a pod's phase/restarts/first-failure reason.
func diagFromPod(pod *corev1.Pod) diag {
	d := diag{phase: string(pod.Status.Phase)}
	for _, cs := range pod.Status.ContainerStatuses {
		d.restarts += cs.RestartCount
		if d.message == "" && cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			d.message = cs.State.Waiting.Reason
			if cs.State.Waiting.Message != "" {
				d.message += ": " + cs.State.Waiting.Message
			}
		}
		if d.message == "" && cs.State.Terminated != nil && cs.State.Terminated.Reason != "" &&
			cs.State.Terminated.Reason != "Completed" {
			d.message = cs.State.Terminated.Reason
		}
	}
	return d
}
