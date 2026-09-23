package servicesmgr

import (
	"bufio"
	"context"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// k8sLogSource reads a service's container logs directly from the Kubernetes
// pod log API. It is the default LogSource; a centralized backend (Loki, ...)
// can replace it without touching the RPC or webui.
type k8sLogSource struct{ c *Client }

// LogSource returns the client's log reader.
func (c *Client) LogSource() LogSource { return k8sLogSource{c: c} }

// newestPod returns the most recently created pod for a service (the active
// replica). Returns apierrors NotFound when none exists.
func (c *Client) newestPod(ctx context.Context, name string) (string, error) {
	pods, err := c.cs.CoreV1().Pods(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=" + name})
	if err != nil {
		return "", err
	}
	if len(pods.Items) == 0 {
		return "", &apierrors.StatusError{ErrStatus: metav1.Status{
			Reason: metav1.StatusReasonNotFound, Message: "no pod for service " + name,
		}}
	}
	newest := pods.Items[0]
	for i := range pods.Items {
		if pods.Items[i].CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = pods.Items[i]
		}
	}
	return newest.Name, nil
}

// containerName is the single app container every managed service runs.
const containerName = "svc"

func (s k8sLogSource) logOpts(name string, opts LogOptions) (*corev1.PodLogOptions, error) {
	o := &corev1.PodLogOptions{Container: containerName, Follow: opts.Follow}
	if opts.TailLines > 0 {
		n := opts.TailLines
		o.TailLines = &n
	}
	if opts.Previous {
		o.Previous = true
	}
	return o, nil
}

// Tail reads up to opts.TailLines lines of the service's current (or previous)
// container log.
func (s k8sLogSource) Tail(ctx context.Context, name string, opts LogOptions) ([]string, error) {
	pod, err := s.c.newestPod(ctx, name)
	if err != nil {
		return nil, err
	}
	o, _ := s.logOpts(name, opts)
	o.Follow = false
	req := s.c.cs.CoreV1().Pods(s.c.namespace).GetLogs(pod, o)
	rc, err := req.Stream(ctx)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	var lines []string
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, strings.TrimRight(sc.Text(), "\r"))
	}
	return lines, sc.Err()
}

// Follow streams the service's container log until ctx is cancelled or the
// container exits. Lines are delivered on the first channel; a terminal error
// (if any) on the second.
func (s k8sLogSource) Follow(ctx context.Context, name string, opts LogOptions) (<-chan string, <-chan error) {
	lines := make(chan string, 256)
	errc := make(chan error, 1)
	go func() {
		defer close(lines)
		defer close(errc)
		pod, err := s.c.newestPod(ctx, name)
		if err != nil {
			errc <- err
			return
		}
		o, _ := s.logOpts(name, opts)
		o.Follow = true
		req := s.c.cs.CoreV1().Pods(s.c.namespace).GetLogs(pod, o)
		rc, err := req.Stream(ctx)
		if err != nil {
			errc <- err
			return
		}
		defer rc.Close()
		sc := bufio.NewScanner(rc)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			select {
			case <-ctx.Done():
				return
			case lines <- strings.TrimRight(sc.Text(), "\r"):
			}
		}
		if err := sc.Err(); err != nil && ctx.Err() == nil {
			errc <- err
		}
	}()
	return lines, errc
}
