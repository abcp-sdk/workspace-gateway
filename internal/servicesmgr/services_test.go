package servicesmgr

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func newTestClient() *Client {
	return NewWithClientset(fake.NewSimpleClientset(), Config{Namespace: "worker"})
}

func TestDeployListDelete(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()

	svc, err := c.Deploy(ctx, Spec{Name: "app", Image: "img:1", Creator: "t/s", ContainerPort: 8080, ServicePort: 80})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if svc.Name != "app" || svc.URL == "" || svc.Replicas != 1 {
		t.Fatalf("bad service: %+v", svc)
	}

	list, err := c.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v, %v", list, err)
	}

	// Idempotent redeploy.
	if _, err := c.Deploy(ctx, Spec{Name: "app", Image: "img:2", Creator: "t/s"}); err != nil {
		t.Fatalf("redeploy: %v", err)
	}
	got, _ := c.Get(ctx, "app")
	if got.Image != "img:2" {
		t.Fatalf("image not updated: %+v", got)
	}

	ok, err := c.Delete(ctx, "app")
	if err != nil || !ok {
		t.Fatalf("delete = %v, %v", ok, err)
	}
	if list, _ := c.List(ctx); len(list) != 0 {
		t.Fatalf("still present: %+v", list)
	}
}

func TestCorePortsGrouping(t *testing.T) {
	// tcp80 + tcp443 + udp443 in one Service (unique names, 443/tcp + 443/udp coexist).
	p := corePorts([]Port{
		{Port: 80, Protocol: "tcp", TargetPort: 8080},
		{Port: 443, Protocol: "tcp", TargetPort: 8443},
		{Port: 443, Protocol: "udp", TargetPort: 8443},
	})
	if len(p) != 3 {
		t.Fatalf("ports = %+v", p)
	}
	if p[0].Port != 80 || p[0].Protocol != corev1.ProtocolTCP {
		t.Fatalf("first = %+v", p[0])
	}
	if p[2].Protocol != corev1.ProtocolUDP || p[2].Port != 443 {
		t.Fatalf("udp443 = %+v", p[2])
	}
}

func TestDeployDefaultExposesPort80(t *testing.T) {
	c := newTestClient()
	// No ports => default single tcp80 -> container port.
	if _, err := c.Deploy(context.Background(), Spec{Name: "app", Image: "img:1", Creator: "t", ContainerPort: 8080}); err != nil {
		t.Fatal(err)
	}
	svc, err := c.cs.CoreV1().Services("worker").Get(context.Background(), "app", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 80 || svc.Spec.Ports[0].TargetPort.IntVal != 8080 {
		t.Fatalf("ports = %+v", svc.Spec.Ports)
	}
}

func TestDeployMultiPortAndSibling(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	// primary: tcp80 + udp443; sibling "admin": tcp80 -> 8081.
	_, err := c.Deploy(ctx, Spec{Name: "app", Image: "img:1", Creator: "t", ContainerPort: 8080, Ports: []Port{
		{Port: 80, Protocol: "tcp", TargetPort: 8080},
		{Port: 443, Protocol: "udp", TargetPort: 8080},
		{Suffix: "admin", Port: 80, Protocol: "tcp", TargetPort: 8081},
	}})
	if err != nil {
		t.Fatal(err)
	}
	primary, _ := c.cs.CoreV1().Services("worker").Get(ctx, "app", metav1.GetOptions{})
	if len(primary.Spec.Ports) != 2 {
		t.Fatalf("primary ports = %+v", primary.Spec.Ports)
	}
	sib, err := c.cs.CoreV1().Services("worker").Get(ctx, "app-admin", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("sibling missing: %v", err)
	}
	if len(sib.Spec.Ports) != 1 || sib.Spec.Ports[0].TargetPort.IntVal != 8081 {
		t.Fatalf("sibling ports = %+v", sib.Spec.Ports)
	}
	// Delete removes both services.
	if ok, err := c.Delete(ctx, "app"); err != nil || !ok {
		t.Fatalf("delete = %v %v", ok, err)
	}
	if _, err := c.cs.CoreV1().Services("worker").Get(ctx, "app-admin", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("sibling not deleted: %v", err)
	}
}

func TestStageAndPreviewReap(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	// A preview service with a past TTL, and a release service.
	if _, err := c.Deploy(ctx, Spec{Name: "prev", Image: "i:1", Creator: "t", Session: "t:x:y", Stage: StagePreview, ExpiresAt: 1000}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Deploy(ctx, Spec{Name: "rel", Image: "i:1", Creator: "t", Session: "t:x:y"}); err != nil {
		t.Fatal(err)
	}
	got, _ := c.Get(ctx, "prev")
	if got.Stage != StagePreview || got.ExpiresAt != 1000 {
		t.Fatalf("stage/ttl not stamped: %+v", got)
	}
	rel, _ := c.Get(ctx, "rel")
	if rel.Stage != StageRelease {
		t.Fatalf("legacy stage = %q, want release", rel.Stage)
	}
	// now=2000 reaps only the expired preview.
	names, err := c.ReapExpired(ctx, 2000)
	if err != nil || len(names) != 1 || names[0] != "prev" {
		t.Fatalf("reap = %v, %v", names, err)
	}
	if _, err := c.Get(ctx, "rel"); err != nil {
		t.Fatal("release service must survive the preview reaper")
	}
}

func TestDeleteBySessionOnlyPreview(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if _, err := c.Deploy(ctx, Spec{Name: "p", Image: "i", Creator: "t", Session: "s1", Stage: StagePreview}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Deploy(ctx, Spec{Name: "r", Image: "i", Creator: "t", Session: "s1", Stage: StageRelease}); err != nil {
		t.Fatal(err)
	}
	names, err := c.DeleteBySession(ctx, "s1", true)
	if err != nil || len(names) != 1 || names[0] != "p" {
		t.Fatalf("onlyPreview delete = %v, %v", names, err)
	}
	if _, err := c.Get(ctx, "r"); err != nil {
		t.Fatal("release must survive a preview-only cleanup")
	}
	// Only-preview=false removes the release too.
	names, _ = c.DeleteBySession(ctx, "s1", false)
	if len(names) != 1 || names[0] != "r" {
		t.Fatalf("full delete = %v", names)
	}
}

func TestDeployRefusesForeignService(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	// A pre-existing Service NOT managed by us, sharing the target name.
	foreign := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "app-admin", Namespace: "worker"},
	}
	if _, err := c.cs.CoreV1().Services("worker").Create(ctx, foreign, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := c.Deploy(ctx, Spec{Name: "app", Image: "img:1", Creator: "t", ContainerPort: 8080, Ports: []Port{
		{Suffix: "admin", Port: 80, Protocol: "tcp", TargetPort: 8080},
	}})
	if err == nil {
		t.Fatal("expected refusal to clobber a foreign Service")
	}
}
