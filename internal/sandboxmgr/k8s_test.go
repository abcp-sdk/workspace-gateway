package sandboxmgr

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func newTestClient() *Client {
	return NewWithClientset(fake.NewSimpleClientset(), Config{Namespace: "worker"})
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

func TestCreateIsIdempotent(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	_, t1, _ := c.Create(ctx, Spec{Name: "x", Image: "i"})
	_, t2, err := c.Create(ctx, Spec{Name: "x", Image: "i"})
	if err != nil {
		t.Fatalf("recreate: %v", err)
	}
	if t1 == t2 {
		t.Fatal("expected a fresh token on recreate")
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
