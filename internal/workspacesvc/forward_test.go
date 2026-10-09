package workspacesvc

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeCloser struct {
	mu     sync.Mutex
	closed bool
}

func (f *fakeCloser) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

func (f *fakeCloser) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// A client disconnect (ctx cancel) must close the upstream stream promptly, so
// the forwarded RPC is canceled and the agent reclaims its consumer.
func TestWatchUpstreamClosesOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fc := &fakeCloser{}
	stop := watchUpstream(ctx, fc)
	defer stop()

	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for !fc.isClosed() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !fc.isClosed() {
		t.Fatal("upstream not closed after ctx cancel")
	}
}

// A normal return (no cancel) must NOT close the upstream out from under the
// caller's own deferred up.Close(); stop() just ends the watcher.
func TestWatchUpstreamStopWithoutCancel(t *testing.T) {
	fc := &fakeCloser{}
	stop := watchUpstream(context.Background(), fc)
	stop() // must not panic; must not close
	time.Sleep(20 * time.Millisecond)
	if fc.isClosed() {
		t.Fatal("upstream closed despite no cancel")
	}
}
