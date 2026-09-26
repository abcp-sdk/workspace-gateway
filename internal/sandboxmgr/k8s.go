// Package sandboxmgr is the gateway's Kubernetes backend: it creates, lists,
// resolves and deletes agent-worker sandboxes as Pod + Service + Secret triples
// in a single namespace. It knows nothing about repos, CI or easylab.
package sandboxmgr

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/abcp-sdk/workspace-gateway/internal/runtimeprofiles"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Labels/annotations the manager stamps on every object it owns.
const (
	LabelManaged = "worker-manager/sandbox"
	LabelName    = "worker-manager/name"
	AnnoImage    = "worker-manager/image"
	AnnoCreator  = "worker-manager/creator"
	// AnnoSession binds a sandbox to the session that created it (its owner),
	// so a session can enumerate and fan out to its own sandboxes.
	AnnoSession = "worker-manager/session"

	// WorkerPort is the agent-worker listen port baked into preset images.
	WorkerPort = 48080
)

// ErrExists reports that a sandbox with the requested name already exists. A
// sandbox name is never reused: creating over a live one would silently destroy
// its workload and token, so callers must delete it first.
var ErrExists = errors.New("sandbox already exists")

// Sandbox is a live worker view.
type Sandbox struct {
	Name      string
	Image     string
	Phase     string
	Ready     bool
	URL       string
	Creator   string
	Session   string
	CreatedAt int64 // unix millis
}

// Spec describes a sandbox to create.
type Spec struct {
	Name    string
	Image   string
	CPU     string
	Memory  string
	Env     map[string]string
	Creator string
	// Session binds the sandbox to its owning session (empty = unbound).
	Session string
	// Runtime is the rendered runtime capabilities (device limits, security
	// context, tun mount, ...). Zero value = a plain sandbox.
	Runtime runtimeprofiles.Rendered
}

// Client wraps the typed clientset plus the target namespace.
type Client struct {
	cs        kubernetes.Interface
	namespace string
}

// Config configures a Client.
type Config struct {
	Namespace string
}

// New builds a client from the in-cluster config (production) or, when
// KUBECONFIG is set and no in-cluster token exists, from the kubeconfig (dev).
func New(cfg Config) (*Client, error) {
	rc, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			return nil, fmt.Errorf("k8s: no in-cluster config and KUBECONFIG unset: %w", err)
		}
		rc, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("k8s: kubeconfig: %w", err)
		}
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("k8s: clientset: %w", err)
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

// Namespace returns the namespace the manager operates in.
func (c *Client) Namespace() string { return c.namespace }

var nameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// resourceName maps a logical sandbox name to a DNS-1123 object name, hashing
// when the input is not already valid (so arbitrary names never collide).
func resourceName(name string) string {
	base := strings.ToLower(name)
	if len(base) <= 40 && nameRe.MatchString(base) {
		return "wm-" + base
	}
	sum := sha256.Sum256([]byte(name))
	return "wm-" + hex.EncodeToString(sum[:])[:16]
}

// newToken mints a 32-byte hex bearer token.
func newToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func defaultResources(cpu, mem string) corev1.ResourceRequirements {
	if cpu == "" {
		cpu = "500m"
	}
	if mem == "" {
		mem = "1Gi"
	}
	rl := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cpu),
		corev1.ResourceMemory: resource.MustParse(mem),
	}
	return corev1.ResourceRequirements{Requests: rl, Limits: rl}
}

// ServiceDNS returns the in-cluster Service URL for a sandbox.
func (c *Client) ServiceDNS(resource string) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", resource, c.namespace, WorkerPort)
}

// Create creates the Pod + Service + Secret for a sandbox and returns the
// sandbox plus its bearer token. It does NOT wait for readiness (the caller
// does, via WaitReady).
func (c *Client) Create(ctx context.Context, s Spec) (Sandbox, string, error) {
	if s.Name == "" || s.Image == "" {
		return Sandbox{}, "", fmt.Errorf("name and image required")
	}
	res := resourceName(s.Name)
	token, err := newToken()
	if err != nil {
		return Sandbox{}, "", err
	}

	labels := map[string]string{
		LabelManaged: "1",
		LabelName:    s.Name,
	}
	annotations := map[string]string{
		AnnoImage:   s.Image,
		AnnoCreator: s.Creator,
		AnnoSession: s.Session,
	}

	// A name is NEVER reused: creating over an existing sandbox would silently
	// destroy a running workload (and its token). The K8s Create calls below are
	// the authoritative guard — an already-present object yields ErrExists and we
	// leave the existing sandbox untouched.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: res, Namespace: c.namespace, Labels: labels},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"token": []byte(token)},
	}
	if _, err := c.cs.CoreV1().Secrets(c.namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return Sandbox{}, "", fmt.Errorf("%w: %s", ErrExists, s.Name)
		}
		return Sandbox{}, "", fmt.Errorf("create secret: %w", err)
	}

	env := []corev1.EnvVar{
		{Name: "WORKER_TOKEN", Value: token},
		{Name: "WORKER_PORT", Value: fmt.Sprintf("%d", WorkerPort)},
	}
	for k, v := range s.Env {
		env = append(env, corev1.EnvVar{Name: k, Value: v})
	}

	// Runtime profile: extra env, devices, caps, resources, tun mount.
	for k, v := range s.Runtime.Env {
		env = append(env, corev1.EnvVar{Name: k, Value: v})
	}
	resources := defaultResources(s.CPU, s.Memory)
	if s.Runtime.Resources != nil {
		resources = *s.Runtime.Resources
	}
	for k, v := range s.Runtime.DeviceLimits {
		if resources.Limits == nil {
			resources.Limits = corev1.ResourceList{}
		}
		resources.Limits[corev1.ResourceName(k)] = resource.MustParse(v)
	}
	container := corev1.Container{
		Name:            "worker",
		Image:           s.Image,
		Env:             env,
		Ports:           []corev1.ContainerPort{{Name: "worker", ContainerPort: WorkerPort}},
		Resources:       resources,
		SecurityContext: s.Runtime.SecurityContext,
	}
	podSpec := corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyAlways,
		Containers:    []corev1.Container{container},
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
		podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
			Name: "devtun",
			VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
				Path: "/dev/net/tun", Type: hostPathTypePtr(corev1.HostPathCharDev),
			}},
		})
		podSpec.Containers[0].VolumeMounts = append(podSpec.Containers[0].VolumeMounts, corev1.VolumeMount{
			Name: "devtun", MountPath: "/dev/net/tun",
		})
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: res, Namespace: c.namespace, Labels: labels, Annotations: annotations,
		},
		Spec: podSpec,
	}
	if _, err := c.cs.CoreV1().Pods(c.namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		_ = c.cs.CoreV1().Secrets(c.namespace).Delete(ctx, res, metav1.DeleteOptions{})
		if apierrors.IsAlreadyExists(err) {
			return Sandbox{}, "", fmt.Errorf("%w: %s", ErrExists, s.Name)
		}
		return Sandbox{}, "", fmt.Errorf("create pod: %w", err)
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: res, Namespace: c.namespace, Labels: labels},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Name: "worker", Port: WorkerPort, TargetPort: intstr.FromInt32(WorkerPort),
			}},
		},
	}
	if _, err := c.cs.CoreV1().Services(c.namespace).Create(ctx, svc, metav1.CreateOptions{}); err != nil {
		_ = c.cs.CoreV1().Pods(c.namespace).Delete(ctx, res, metav1.DeleteOptions{})
		_ = c.cs.CoreV1().Secrets(c.namespace).Delete(ctx, res, metav1.DeleteOptions{})
		if apierrors.IsAlreadyExists(err) {
			return Sandbox{}, "", fmt.Errorf("%w: %s", ErrExists, s.Name)
		}
		return Sandbox{}, "", fmt.Errorf("create service: %w", err)
	}

	sb := Sandbox{
		Name: s.Name, Image: s.Image, Phase: "Pending",
		URL: c.ServiceDNS(res), Creator: s.Creator, Session: s.Session,
		CreatedAt: time.Now().UnixMilli(),
	}
	return sb, token, nil
}

// Get returns one sandbox by logical name.
func (c *Client) Get(ctx context.Context, name string) (Sandbox, bool, error) {
	res := resourceName(name)
	pod, err := c.cs.CoreV1().Pods(c.namespace).Get(ctx, res, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Sandbox{}, false, nil
	}
	if err != nil {
		return Sandbox{}, false, err
	}
	return toSandbox(c, pod), true, nil
}

// List returns every sandbox the manager owns in its namespace.
func (c *Client) List(ctx context.Context) ([]Sandbox, error) {
	pods, err := c.cs.CoreV1().Pods(c.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelManaged + "=1",
	})
	if err != nil {
		return nil, err
	}
	out := make([]Sandbox, 0, len(pods.Items))
	for i := range pods.Items {
		out = append(out, toSandbox(c, &pods.Items[i]))
	}
	return out, nil
}

// Delete removes the Pod + Service + Secret for a sandbox and waits (bounded)
// for the pod to actually disappear, so an immediate List does not still show
// it as Terminating.
func (c *Client) Delete(ctx context.Context, name string) (bool, error) {
	res := resourceName(name)
	// Only delete something we own (avoid nuking an unrelated object that
	// happens to share the name).
	if _, err := c.cs.CoreV1().Pods(c.namespace).Get(ctx, res, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if err := c.deleteObjects(ctx, res); err != nil {
		return false, err
	}
	// Wait for the pod to be gone (deletion is asynchronous).
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := c.cs.CoreV1().Pods(c.namespace).Get(ctx, res, metav1.GetOptions{}); apierrors.IsNotFound(err) {
			return true, nil
		}
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return true, nil
}

// Resolve returns the URL + bearer token for a sandbox.
func (c *Client) Resolve(ctx context.Context, name string) (string, string, error) {
	res := resourceName(name)
	sec, err := c.cs.CoreV1().Secrets(c.namespace).Get(ctx, res, metav1.GetOptions{})
	if err != nil {
		return "", "", fmt.Errorf("resolve %s: %w", name, err)
	}
	token := string(sec.Data["token"])
	return c.ServiceDNS(res), token, nil
}

func (c *Client) deleteObjects(ctx context.Context, res string) error {
	_ = c.cs.CoreV1().Services(c.namespace).Delete(ctx, res, metav1.DeleteOptions{})
	_ = c.cs.CoreV1().Secrets(c.namespace).Delete(ctx, res, metav1.DeleteOptions{})
	err := c.cs.CoreV1().Pods(c.namespace).Delete(ctx, res, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func toSandbox(c *Client, p *corev1.Pod) Sandbox {
	ready := false
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Ready {
			ready = true
		}
	}
	phase := string(p.Status.Phase)
	if phase == "" {
		phase = "Unknown"
	}
	created := p.CreationTimestamp.UnixMilli()
	if created == 0 {
		created = time.Now().UnixMilli()
	}
	return Sandbox{
		Name:      p.Labels[LabelName],
		Image:     p.Annotations[AnnoImage],
		Phase:     phase,
		Ready:     ready,
		URL:       c.ServiceDNS(p.Name),
		Creator:   p.Annotations[AnnoCreator],
		Session:   p.Annotations[AnnoSession],
		CreatedAt: created,
	}
}

func hostPathTypePtr(t corev1.HostPathType) *corev1.HostPathType { return &t }
