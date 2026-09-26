package workspacesvc

import (
	"context"
	"sync"

	"github.com/abcp-sdk/workspace-gateway/internal/k8swatch"
)

// workspaceHub fans out a single "workspace changed" signal to every connected
// WatchWorkspace subscriber. It owns ONE k8s watch per resource (pods /
// deployments / claims), started when the first subscriber joins and stopped
// when the last leaves, so an idle deployment runs no watches.
//
// Subscribers re-list on each signal (they need tenant filtering anyway), so
// the hub carries no per-tenant data — just a coalesced wake-up.
type workspaceHub struct {
	mu      sync.Mutex
	subs    map[chan struct{}]struct{}
	cancel  context.CancelFunc
	started bool
	sources []k8swatch.Source
}

func newWorkspaceHub(sources ...k8swatch.Source) *workspaceHub {
	return &workspaceHub{subs: map[chan struct{}]struct{}{}, sources: sources}
}

// subscribe registers a new subscriber and starts the watches on the first
// one. The returned channel is closed when `unsub` is called.
func (h *workspaceHub) subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	if !h.started {
		ctx, cancel := context.WithCancel(context.Background())
		h.cancel = cancel
		h.started = true
		go h.run(ctx)
	}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
		if len(h.subs) == 0 && h.started {
			h.cancel()
			h.started = false
		}
		h.mu.Unlock()
	}
}

// run forwards coalesced change signals to every subscriber.
func (h *workspaceHub) run(ctx context.Context) {
	changes := k8swatch.Changes(ctx, h.sources...)
	for range changes {
		h.mu.Lock()
		for ch := range h.subs {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
		h.mu.Unlock()
	}
}
