// Package k8swatch coalesces Kubernetes watch events from several resources
// into a single "something changed" signal channel. It is the push primitive
// behind the gateway's WatchWorkspace stream: the managers expose the raw
// resources, and the gateway re-lists + pushes a frame whenever any changes.
package k8swatch

import (
	"context"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/watch"
)

// Source opens one watch. It must return a fresh watch each call (the caller
// re-invokes it after the previous one closes or errors).
type Source func(ctx context.Context) (watch.Interface, error)

// Changes returns a channel that receives a coalesced signal whenever any of
// `sources` emits an event. Each source is re-established with a short backoff
// after it closes or errors, until `ctx` is cancelled (then the channel is
// closed). The channel has capacity 1 and drops duplicate pending signals, so
// a burst of events collapses into a single wake-up.
func Changes(ctx context.Context, sources ...Source) <-chan struct{} {
	out := make(chan struct{}, 1)
	signal := func() {
		select {
		case out <- struct{}{}:
		default:
		}
	}
	var wg sync.WaitGroup
	for _, src := range sources {
		wg.Add(1)
		go func(src Source) {
			defer wg.Done()
			for ctx.Err() == nil {
				w, err := src(ctx)
				if err != nil {
					select {
					case <-ctx.Done():
						return
					case <-time.After(time.Second):
					}
					continue
				}
				drain(ctx, w, signal)
				w.Stop()
			}
		}(src)
	}
	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}

// drain forwards every event from `w` as a coalesced signal until the watch
// closes or ctx is cancelled.
func drain(ctx context.Context, w watch.Interface, signal func()) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.ResultChan():
			if !ok {
				return
			}
			// Bookmark/error events still wake the caller: it re-lists and
			// re-establishes, so a missed change is still observed.
			_ = ev
			signal()
		}
	}
}
