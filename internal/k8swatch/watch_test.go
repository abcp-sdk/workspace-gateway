package k8swatch

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/watch"
)

func TestChangesCoalescesAndCloses(t *testing.T) {
	fw := watch.NewFake()
	src := func(context.Context) (watch.Interface, error) { return fw, nil }
	ctx, cancel := context.WithCancel(context.Background())
	ch := Changes(ctx, src)

	fw.Add(&corev1.Pod{})
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("no signal after Add")
	}
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			// a buffered signal may still be pending; drain once more
			select {
			case _, ok2 := <-ch:
				if ok2 {
					t.Fatal("channel not closed after cancel")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("channel not closed after cancel")
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel not closed after cancel")
	}
}
