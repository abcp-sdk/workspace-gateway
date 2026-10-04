package sandboxmgr

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/abcp-sdk/workspace-gateway/internal/runtimeprofiles"
)

func newTestClient() *Client {
	return NewWithClientset(fake.NewSimpleClientset(), Config{Namespace: "worker"})
}

func TestCreateSetsRuntimeClass(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	// A GPU profile renders a RuntimeClass; the pod must carry it, or the
	// driver devices are never mounted.
	_, _, err := c.Create(ctx, Spec{
		Name: "gpu", Image: "img:1", Creator: "alice",
		Runtime: runtimeprofiles.DefaultSettings().Render(false, 1),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	pod, err := c.cs.CoreV1().Pods("worker").Get(ctx, resourceName("gpu"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != "nvidia" {
		t.Fatalf("runtimeClassName = %v, want nvidia", pod.Spec.RuntimeClassName)
	}
	if got := pod.Spec.Containers[0].Resources.Limits["nvidia.com/gpu"]; got.String() != "1" {
		t.Fatalf("gpu limit = %v", got)
	}
}

func TestCreateAppliesBootstrap(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	_, _, err := c.Create(ctx, Spec{
		Name: "boot", Image: "img:1", Creator: "alice",
		Env: map[string]string{"SANDBOX_PACKAGE_UPSTREAM": "http://artifact"},
		Bootstrap: &Bootstrap{
			Image:  "alpine:3.24",
			Script: "echo hi",
			Mounts: []BootstrapMount{
				{Name: "cfg-apt", Path: "/etc/apt/sources.list.d"},
				{Name: "cfg-m2", Path: "/root/.m2", Writable: true},
				{Name: "f-gemrc", Path: "/root/.gemrc", File: true},
			},
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	pod, err := c.cs.CoreV1().Pods("worker").Get(ctx, resourceName("boot"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pod.Spec.InitContainers) != 1 {
		t.Fatalf("init containers = %d, want 1", len(pod.Spec.InitContainers))
	}
	ic := pod.Spec.InitContainers[0]
	if ic.Image != "alpine:3.24" || len(ic.VolumeMounts) != 3 {
		t.Fatalf("init container = %+v", ic)
	}
	// The init container must NOT receive the worker's bearer token.
	for _, e := range ic.Env {
		if e.Name == "WORKER_TOKEN" || e.Name == "WORKER_PORT" {
			t.Fatalf("init container leaked worker env %q", e.Name)
		}
	}
	// Volumes exist and the worker mounts them; the Writable mount is NOT RO.
	vols := map[string]bool{}
	for _, v := range pod.Spec.Volumes {
		vols[v.Name] = v.EmptyDir != nil
	}
	if !vols["cfg-apt"] || !vols["cfg-m2"] {
		t.Fatalf("missing emptyDir volumes: %v", vols)
	}
	ro := map[string]bool{}
	sub := map[string]string{}
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == "cfg-apt" || m.Name == "cfg-m2" || m.Name == "f-gemrc" {
			ro[m.Name] = m.ReadOnly
			sub[m.Name] = m.SubPath
		}
	}
	if !ro["cfg-apt"] {
		t.Error("cfg-apt should be read-only")
	}
	if ro["cfg-m2"] {
		t.Error("cfg-m2 must be read-WRITE (Maven repo root)")
	}
	// A FILE mount uses subPath (its parent dir must NOT be overlaid).
	if sub["f-gemrc"] != "cfg" || !ro["f-gemrc"] {
		t.Errorf("f-gemrc must be a read-only subPath mount, got subPath=%q ro=%v", sub["f-gemrc"], ro["f-gemrc"])
	}
}

func TestCreateListResolveDelete(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()

	sb, token, err := c.Create(ctx, Spec{Name: "dev", Image: "img:1", Creator: "alice"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if token == "" {
		t.Fatal("empty token")
	}
	if sb.Name != "dev" || sb.URL == "" {
		t.Fatalf("bad sandbox: %+v", sb)
	}

	// Secret carries the token.
	sec, err := c.cs.CoreV1().Secrets("worker").Get(ctx, resourceName("dev"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("secret: %v", err)
	}
	if string(sec.Data["token"]) != token {
		t.Fatalf("secret token mismatch")
	}

	// Pod has WORKER_TOKEN + port env.
	pod, err := c.cs.CoreV1().Pods("worker").Get(ctx, resourceName("dev"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("pod: %v", err)
	}
	envs := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		envs[e.Name] = e.Value
	}
	if envs["WORKER_TOKEN"] != token || envs["WORKER_PORT"] != "48080" {
		t.Fatalf("bad env: %+v", envs)
	}
	if pod.Annotations[AnnoCreator] != "alice" {
		t.Fatalf("creator annotation missing")
	}

	// Service selects the pod.
	if _, err := c.cs.CoreV1().Services("worker").Get(ctx, resourceName("dev"), metav1.GetOptions{}); err != nil {
		t.Fatalf("service: %v", err)
	}

	// List.
	list, err := c.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v len=%d", err, len(list))
	}

	// Resolve.
	url, tok, err := c.Resolve(ctx, "dev")
	if err != nil || tok != token || url != sb.URL {
		t.Fatalf("resolve: %v tok=%q url=%q", err, tok, url)
	}

	// Get.
	got, ok, err := c.Get(ctx, "dev")
	if err != nil || !ok || got.Name != "dev" {
		t.Fatalf("get: %v ok=%v", err, ok)
	}

	// Delete removes all three.
	ok, err = c.Delete(ctx, "dev")
	if err != nil || !ok {
		t.Fatalf("delete: %v ok=%v", err, ok)
	}
	if _, err := c.cs.CoreV1().Pods("worker").Get(ctx, resourceName("dev"), metav1.GetOptions{}); err == nil {
		t.Fatal("pod still present")
	}
	// Deleting again is a no-op, not an error.
	ok, err = c.Delete(ctx, "dev")
	if err != nil || ok {
		t.Fatalf("second delete: %v ok=%v", err, ok)
	}
}

func TestCreateRefusesExisting(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	_, t1, err := c.Create(ctx, Spec{Name: "x", Image: "i"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// A second create with the same name must be REFUSED, not silently
	// overwrite (which would destroy the running sandbox and rotate its token).
	_, _, err = c.Create(ctx, Spec{Name: "x", Image: "i"})
	if !errors.Is(err, ErrExists) {
		t.Fatalf("recreate err = %v, want ErrExists", err)
	}
	// The original sandbox and token survive untouched.
	got, ok, err := c.Get(ctx, "x")
	if err != nil || !ok {
		t.Fatalf("get after refused recreate: err=%v ok=%v", err, ok)
	}
	if got.Name != "x" {
		t.Fatalf("sandbox name = %q, want x", got.Name)
	}
	_, tok, err := c.Resolve(ctx, "x")
	if err != nil || tok != t1 {
		t.Fatalf("token rotated: err=%v tok=%q want %q", err, tok, t1)
	}
}

func TestResourceNameHashesInvalid(t *testing.T) {
	if resourceName("dev-01") != "wm-dev-01" {
		t.Fatalf("valid name mangled: %s", resourceName("dev-01"))
	}
	a := resourceName("Dev Team/One")
	b := resourceName("Dev Team/Two")
	if a == b || len(a) > 63 {
		t.Fatalf("invalid names not hashed distinctly: %s %s", a, b)
	}
}

func TestResolveMissing(t *testing.T) {
	c := newTestClient()
	if _, _, err := c.Resolve(context.Background(), "nope"); err == nil {
		t.Fatal("expected error for missing worker")
	}
}

func TestGetMissing(t *testing.T) {
	c := newTestClient()
	_, ok, err := c.Get(context.Background(), "nope")
	if err != nil || ok {
		t.Fatalf("get missing: err=%v ok=%v", err, ok)
	}
}

var _ = corev1.Secret{}

func TestDefaultResourcesSplit(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	// Omitted cpu/memory => Burstable: requests 500m/1Gi, limits 2/4Gi.
	if _, _, err := c.Create(ctx, Spec{Name: "dflt", Image: "img:1", Creator: "a"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	pod, err := c.cs.CoreV1().Pods("worker").Get(ctx, resourceName("dflt"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r := pod.Spec.Containers[0].Resources
	if got := r.Requests.Cpu().String(); got != "500m" {
		t.Fatalf("cpu request = %s, want 500m", got)
	}
	if got := r.Limits.Cpu().String(); got != "2" {
		t.Fatalf("cpu limit = %s, want 2", got)
	}
	if got := r.Requests.Memory().String(); got != "1Gi" {
		t.Fatalf("mem request = %s, want 1Gi", got)
	}
	if got := r.Limits.Memory().String(); got != "4Gi" {
		t.Fatalf("mem limit = %s, want 4Gi", got)
	}
	// Default restart policy is Never (a killed sandbox must not self-heal).
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatalf("restartPolicy = %s, want Never", pod.Spec.RestartPolicy)
	}
}

func TestExplicitResourcesKeepRequestEqualLimit(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if _, _, err := c.Create(ctx, Spec{Name: "exp", Image: "img:1", CPU: "1", Memory: "2Gi"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	pod, _ := c.cs.CoreV1().Pods("worker").Get(ctx, resourceName("exp"), metav1.GetOptions{})
	r := pod.Spec.Containers[0].Resources
	if r.Requests.Cpu().String() != "1" || r.Limits.Cpu().String() != "1" {
		t.Fatalf("cpu = %s/%s, want 1/1", r.Requests.Cpu(), r.Limits.Cpu())
	}
	if r.Requests.Memory().String() != "2Gi" || r.Limits.Memory().String() != "2Gi" {
		t.Fatalf("mem = %s/%s, want 2Gi/2Gi", r.Requests.Memory(), r.Limits.Memory())
	}
}
