package servicesmgr

import (
	"context"
	"testing"

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
