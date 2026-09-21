// Package servicesmgr manages long-lived services: a user image run as a
// Kubernetes Deployment + Service in the managed namespace. Unlike sandboxes,
// services get NO worker injection and NO sidecar — they are the caller's app.
package servicesmgr

import (
	"context"
	"fmt"
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
	CreatedAt int64
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

// Deploy creates or updates the Deployment + Service for a spec (idempotent).
func (c *Client) Deploy(ctx context.Context, s Spec) (Service, error) {
	if s.Name == "" || s.Image == "" {
		return Service{}, fmt.Errorf("name and image required")
	}
	if s.ContainerPort == 0 {
		s.ContainerPort = 8080
	}
	if s.ServicePort == 0 {
		s.ServicePort = 80
	}
	reps := s.Replicas
	if reps <= 0 {
		reps = 1
	}
	labels := map[string]string{LabelManaged: "1", LabelName: s.Name, "app": s.Name}
	annotations := map[string]string{AnnoImage: s.Image, AnnoCreator: s.Creator}

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
	container := corev1.Container{
		Name:            "svc",
		Image:           s.Image,
		Env:             env,
		Command:         s.Command,
		Ports:           []corev1.ContainerPort{{Name: "http", ContainerPort: s.ContainerPort}},
		Resources:       rr,
		SecurityContext: s.Runtime.SecurityContext,
	}
	podSpec := corev1.PodSpec{Containers: []corev1.Container{container}}
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

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: s.Name, Namespace: c.namespace, Labels: labels, Annotations: annotations},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": s.Name},
			Ports:    []corev1.ServicePort{{Name: "http", Port: s.ServicePort, TargetPort: intstr.FromInt32(s.ContainerPort)}},
		},
	}
	if err := c.applyService(ctx, svc); err != nil {
		return Service{}, err
	}
	return c.Get(ctx, s.Name)
}

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
	svcPort := int32(80)
	if svc, serr := c.cs.CoreV1().Services(c.namespace).Get(ctx, name, metav1.GetOptions{}); serr == nil && len(svc.Spec.Ports) > 0 {
		svcPort = svc.Spec.Ports[0].Port
	}
	return toService(c, d, svcPort), nil
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
		out = append(out, toService(c, &list.Items[i], 80))
	}
	return out, nil
}

// Delete removes the Deployment + Service.
func (c *Client) Delete(ctx context.Context, name string) (bool, error) {
	_, err := c.cs.AppsV1().Deployments(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	_ = c.cs.CoreV1().Services(c.namespace).Delete(ctx, name, metav1.DeleteOptions{})
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

func toService(c *Client, d *appsv1.Deployment, svcPort int32) Service {
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
	return Service{
		Name: d.Name, Image: d.Annotations[AnnoImage], Phase: phase,
		Ready: d.Status.ReadyReplicas > 0, Replicas: reps,
		URL:     fmt.Sprintf("http://%s:%d", host, svcPort),
		Creator: d.Annotations[AnnoCreator], CreatedAt: d.CreationTimestamp.UnixMilli(),
	}
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
