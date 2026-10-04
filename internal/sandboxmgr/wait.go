package sandboxmgr

import (
	"context"
	"fmt"
	"net"
	"time"

	corev1 "k8s.io/api/core/v1"
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
	// The Service fronts the worker on ServicePort (80); the pod IP is dialed on
	// the worker's own WorkerPort (the container listens on 48080).
	svcAddr := fmt.Sprintf("%s.%s.svc.cluster.local:%d", res, c.namespace, ServicePort)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pod, err := c.cs.CoreV1().Pods(c.namespace).Get(ctx, res, metav1.GetOptions{})
		if err == nil {
			// A failed pod (RestartPolicy Never) never becomes ready: surface the
			// reason immediately instead of waiting out the whole timeout.
			if pod.Status.Phase == "Failed" {
				return fmt.Errorf("sandbox %s exited: %s", name, firstFailureReason(pod))
			}
			if pod.Status.Phase == "Running" && pod.Status.PodIP != "" {
				if dialAddr(ctx, fmt.Sprintf("%s:%d", pod.Status.PodIP, WorkerPort)) == nil &&
					dialAddr(ctx, svcAddr) == nil {
					return nil
				}
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

// firstFailureReason returns the first container's failure reason+message (e.g.
// `OOMKilled`), or a generic message when none is reported.
func firstFailureReason(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Terminated != nil && cs.State.Terminated.Reason != "" {
			msg := cs.State.Terminated.Reason
			if cs.State.Terminated.Message != "" {
				msg += ": " + cs.State.Terminated.Message
			}
			return msg
		}
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			return cs.State.Waiting.Reason
		}
	}
	return "pod failed"
}

func dialAddr(ctx context.Context, addr string) error {
	d := net.Dialer{Timeout: time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}
