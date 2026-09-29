package sandboxmgr

import (
	"bufio"
	"context"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LogOptions selects which sandbox container log to read.
type LogOptions struct {
	// TailLines limits the returned lines (0 = all available in the buffer).
	TailLines int64
	// Previous reads the PREVIOUS container instance (the crash / OOM that
	// terminated it).
	Previous bool
}

// sandboxContainer is the single container every sandbox pod runs.
const sandboxContainer = "worker"

// TailLogs reads up to opts.TailLines lines of a sandbox pod's container log
// (or its previous instance). Returns the lines plus the pod's live phase and
// failure reason, so a container that never produced logs still explains why.
func (c *Client) TailLogs(ctx context.Context, name string, opts LogOptions) ([]string, string, int32, string, error) {
	res := resourceName(name)
	pod, err := c.cs.CoreV1().Pods(c.namespace).Get(ctx, res, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "", 0, "", err
		}
		return nil, "", 0, "", err
	}
	sb := toSandbox(c, pod)
	o := &corev1.PodLogOptions{Container: sandboxContainer}
	if opts.TailLines > 0 {
		n := opts.TailLines
		o.TailLines = &n
	}
	if opts.Previous {
		o.Previous = true
	}
	req := c.cs.CoreV1().Pods(c.namespace).GetLogs(res, o)
	rc, err := req.Stream(ctx)
	if err != nil {
		// No logs (container never started, or no previous instance): return the
		// diagnostics rather than failing the whole call.
		return nil, sb.Phase, sb.Restarts, sb.Message, nil
	}
	defer rc.Close()
	var lines []string
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, strings.TrimRight(sc.Text(), "\r"))
	}
	return lines, sb.Phase, sb.Restarts, sb.Message, sc.Err()
}
