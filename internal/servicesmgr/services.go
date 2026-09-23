// Package servicesmgr manages long-lived services: a user image run as a
// Kubernetes Deployment + Service in the managed namespace. Unlike sandboxes,
// services get NO worker injection and NO sidecar — they are the caller's app.
package servicesmgr

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

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
	// AnnoStage is "release" (long-lived, public) or "preview" (developer
	// verification: session-bound, cluster-only, TTL-reclaimed).
	AnnoStage = "workspace/service-stage"
	// AnnoExpiresAt is the unix-millis deadline after which a preview service is
	// reclaimed (empty/0 = no TTL).
	AnnoExpiresAt = "workspace/service-expires-at"
)

// Stage values for AnnoStage.
const (
	StageRelease = "release"
	StagePreview = "preview"
)

// Service is a live service view.
type Service struct {
	Name      string
	Image     string
	Phase     string
	Ready     bool
	Replicas  int32
	URL       string
	Creator   string
	Session   string
	CreatedAt int64
	// Ports is every port this service exposes (across its primary Service and
	// any sibling `<name>-<suffix>` Services).
	Ports []Port
	// Stage is "release" or "preview" (empty on legacy objects = release).
	Stage string
	// ExpiresAt is the unix-millis deadline for a preview service (0 = none).
	ExpiresAt int64
	// Pod diagnostics (best-effort; zero when no pod is observed).
	PodPhase string
	Restarts int32
	// Message is the waiting/terminated reason of the first unhealthy container
	// (e.g. CrashLoopBackOff, ImagePullBackOff); empty when healthy.
	Message string
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
	// Stage is "release" (default) or "preview".
	Stage string
	// ExpiresAt is the unix-millis TTL deadline for a preview (0 = none).
	ExpiresAt int64
	// Ports are the resolved ports to expose. Empty = the default single public
	// port (tcp80 -> ContainerPort). Entries sharing a Suffix form one Service.
	Ports []Port
	// Runtime is the rendered runtime profile (device limits, security context,
	// tun mount, node selector, ...). Zero value = a plain service.
	Runtime runtimeprofiles.Rendered
}

// Client wraps the typed clientset plus the target namespace.
type Client struct {
	cs        kubernetes.Interface
	namespace string
}

// Config configures a Client.
type Config struct{ Namespace string }

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
	return &Client{cs: cs, namespace: ns}
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
	stage := s.Stage
	if stage == "" {
		stage = StageRelease
	}
	labels := map[string]string{LabelManaged: "1", LabelName: s.Name, "app": s.Name}
	annotations := map[string]string{
		AnnoImage: s.Image, AnnoCreator: s.Creator, AnnoSession: s.Session, AnnoStage: stage,
	}
	if s.ExpiresAt > 0 {
		annotations[AnnoExpiresAt] = fmt.Sprint(s.ExpiresAt)
	}

	env := []corev1.EnvVar{}
	for k, v := range s.Env {
		env = append(env, corev1.EnvVar{Name: k, Value: v})
	}
	for k, v := range s.Runtime.Env {
		env = append(env, corev1.EnvVar{Name: k, Value: v})
	}
	rr := resources(s.CPU, s.Memory)
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
		Command:         s.Command,
		Ports:           containerPorts,
		Resources:       rr,
		SecurityContext: s.Runtime.SecurityContext,
	}
	podSpec := corev1.PodSpec{Containers: []corev1.Container{container}}
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
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: s.Name, Namespace: c.namespace, Labels: labels, Annotations: annotations},
		Spec: appsv1.DeploymentSpec{
			Replicas: &reps,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": s.Name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: annotations},
				Spec:       podSpec,
			},
		},
	}
	if err := c.applyDeployment(ctx, dep); err != nil {
		return Service{}, err
	}

	// One Service per suffix, each selecting the same pod.
	for _, suffix := range order {
		svcName := siblingName(s.Name, suffix)
		svcLabels := map[string]string{LabelManaged: "1", LabelName: s.Name, "app": s.Name}
		svcAnnotations := map[string]string{
			AnnoImage: s.Image, AnnoCreator: s.Creator, AnnoSession: s.Session,
			AnnoStage: stage, AnnoPrimary: s.Name,
		}
		if s.ExpiresAt > 0 {
			svcAnnotations[AnnoExpiresAt] = fmt.Sprint(s.ExpiresAt)
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
		svc := &list.Items[i]
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
	out := make([]Service, 0, len(list.Items))
	for i := range list.Items {
		svc := toService(c, &list.Items[i], c.portsFor(ctx, list.Items[i].Name))
		svc.PodPhase, svc.Restarts, svc.Message = c.podDiagnostics(ctx, list.Items[i].Name)
		out = append(out, svc)
	}
	return out, nil
}

// DeleteBySession removes every service bound to a session. When onlyPreview
// is true, only `preview` services are removed (a release service outlives its
// session). Returns the names deleted.
func (c *Client) DeleteBySession(ctx context.Context, session string, onlyPreview bool) ([]string, error) {
	all, err := c.List(ctx)
	if err != nil {
		return nil, err
	}
	var deleted []string
	for _, s := range all {
		if s.Session != session {
			continue
		}
		if onlyPreview && s.Stage != StagePreview {
			continue
		}
		if ok, derr := c.Delete(ctx, s.Name); derr == nil && ok {
			deleted = append(deleted, s.Name)
		}
	}
	return deleted, nil
}

// ReapExpired deletes preview services whose TTL has passed. Returns the names
// deleted.
func (c *Client) ReapExpired(ctx context.Context, now int64) ([]string, error) {
	all, err := c.List(ctx)
	if err != nil {
		return nil, err
	}
	var deleted []string
	for _, s := range all {
		if s.Stage != StagePreview || s.ExpiresAt <= 0 || now < s.ExpiresAt {
			continue
		}
		if ok, derr := c.Delete(ctx, s.Name); derr == nil && ok {
			deleted = append(deleted, s.Name)
		}
	}
	return deleted, nil
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

func resources(cpu, mem string) corev1.ResourceRequirements {
	if cpu == "" && mem == "" {
		return corev1.ResourceRequirements{}
	}
	rl := corev1.ResourceList{}
	if cpu != "" {
		rl[corev1.ResourceCPU] = resource.MustParse(cpu)
	}
	if mem != "" {
		rl[corev1.ResourceMemory] = resource.MustParse(mem)
	}
	return corev1.ResourceRequirements{Requests: rl, Limits: rl}
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
	stage := d.Annotations[AnnoStage]
	if stage == "" {
		stage = StageRelease
	}
	expires := int64(0)
	if v := d.Annotations[AnnoExpiresAt]; v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			expires = n
		}
	}
	return Service{
		Name: d.Name, Image: d.Annotations[AnnoImage], Phase: phase,
		Ready: d.Status.ReadyReplicas > 0, Replicas: reps,
		URL:     fmt.Sprintf("http://%s:%d", host, urlPort),
		Creator: d.Annotations[AnnoCreator], Session: d.Annotations[AnnoSession],
		CreatedAt: d.CreationTimestamp.UnixMilli(),
		Ports:     ports,
		Stage:     stage, ExpiresAt: expires,
	}
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
