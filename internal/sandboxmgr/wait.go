package sandboxmgr

import (
	"context"
	"fmt"
	"net"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WaitReady polls until the sandbox's Service address accepts a TCP connection
// (the same path every later call uses), or the timeout elapses. It checks both
// the pod IP and the Service DNS to avoid the kube-proxy propagation race.
func (c *Client) WaitReady(ctx context.Context, name string, timeout time.Duration) error {
	res := resourceName(name)
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	svcAddr := fmt.Sprintf("%s.%s.svc.cluster.local:%d", res, c.namespace, WorkerPort)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pod, err := c.cs.CoreV1().Pods(c.namespace).Get(ctx, res, metav1.GetOptions{})
		if err == nil && pod.Status.Phase == "Running" && pod.Status.PodIP != "" {
			if dialAddr(ctx, fmt.Sprintf("%s:%d", pod.Status.PodIP, WorkerPort)) == nil &&
				dialAddr(ctx, svcAddr) == nil {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("sandbox %s not healthy within %s", name, timeout)
}

func dialAddr(ctx context.Context, addr string) error {
	d := net.Dialer{Timeout: time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}
