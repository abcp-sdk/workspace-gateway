package helmmgr

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ObjectStatus is the LIVE status of one object a release deployed.
type ObjectStatus struct {
	Kind       string
	Name       string
	Namespace  string
	Status     string // Ready | Progressing | Failed | Complete | NotFound | <Pod phase>
	Ready      bool
	Message    string // first unhealthy container's reason (e.g. CrashLoopBackOff)
	Phase      string // Pod phase
	Restarts   int32
	ReadyReps  int32
	DesiredRep int32
}

// Objects returns the live status of the release's current-revision objects,
// resolving Pods (via the owning workload's selector) so per-pod health and
// logs are reachable without the caller knowing the object names up front.
func (c *Client) Objects(ctx context.Context, release string) ([]ObjectStatus, error) {
	rel, ok, err := c.Get(ctx, release)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("release %q not found", release)
	}
	out := make([]ObjectStatus, 0, len(rel.Objects))
	for _, o := range rel.Objects {
		st := ObjectStatus{Kind: o.Kind, Name: o.Name, Namespace: c.namespace}
		c.fillStatus(ctx, &st)
		out = append(out, st)
	}
	return out, nil
}

// fillStatus resolves one object's live status by kind.
func (c *Client) fillStatus(ctx context.Context, st *ObjectStatus) {
	ns := c.namespace
	switch strings.ToLower(st.Kind) {
	case "pod":
		p, err := c.cs.CoreV1().Pods(ns).Get(ctx, st.Name, metav1.GetOptions{})
		if err != nil {
			st.Status, st.Message = "NotFound", errReason(err)
			return
		}
		applyPodStatus(st, p)
	case "deployment":
		d, err := c.cs.AppsV1().Deployments(ns).Get(ctx, st.Name, metav1.GetOptions{})
		if err != nil {
			st.Status, st.Message = "NotFound", errReason(err)
			return
		}
		desired := int32(0)
		if d.Spec.Replicas != nil {
			desired = *d.Spec.Replicas
		}
		st.DesiredRep, st.ReadyReps = desired, d.Status.ReadyReplicas
		st.Ready = desired > 0 && d.Status.ReadyReplicas >= desired
		st.Status = phase(st.Ready, desired, d.Status.ReadyReplicas)
	case "statefulset":
		s, err := c.cs.AppsV1().StatefulSets(ns).Get(ctx, st.Name, metav1.GetOptions{})
		if err != nil {
			st.Status, st.Message = "NotFound", errReason(err)
			return
		}
		desired := int32(0)
		if s.Spec.Replicas != nil {
			desired = *s.Spec.Replicas
		}
		st.DesiredRep, st.ReadyReps = desired, s.Status.ReadyReplicas
		st.Ready = desired > 0 && s.Status.ReadyReplicas >= desired
		st.Status = phase(st.Ready, desired, s.Status.ReadyReplicas)
	case "daemonset":
		d, err := c.cs.AppsV1().DaemonSets(ns).Get(ctx, st.Name, metav1.GetOptions{})
		if err != nil {
			st.Status, st.Message = "NotFound", errReason(err)
			return
		}
		st.DesiredRep, st.ReadyReps = d.Status.DesiredNumberScheduled, d.Status.NumberReady
		st.Ready = d.Status.DesiredNumberScheduled > 0 && d.Status.NumberReady >= d.Status.DesiredNumberScheduled
		st.Status = phase(st.Ready, d.Status.DesiredNumberScheduled, d.Status.NumberReady)
	case "job":
		j, err := c.cs.BatchV1().Jobs(ns).Get(ctx, st.Name, metav1.GetOptions{})
		if err != nil {
			st.Status, st.Message = "NotFound", errReason(err)
			return
		}
		st.Ready = j.Status.Succeeded > 0
		st.Status = "Complete"
		if !st.Ready {
			st.Status = "Progressing"
		}
	case "service", "configmap", "secret", "persistentvolumeclaim", "serviceaccount":
		st.Status, st.Ready = "Ready", true
	default:
		// Unknown kinds are reported as present but unassessed.
		st.Status = "Ready"
	}
}

// applyPodStatus fills a Pod's phase/restarts/readiness + the first unhealthy
// container's reason (so a CrashLoopBackOff explains itself).
func applyPodStatus(st *ObjectStatus, p *corev1.Pod) {
	st.Phase = string(p.Status.Phase)
	st.Status = string(p.Status.Phase)
	ready := true
	for _, cs := range p.Status.ContainerStatuses {
		st.Restarts += cs.RestartCount
		if !cs.Ready {
			ready = false
		}
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			st.Message = cs.State.Waiting.Reason
			if cs.State.Waiting.Message != "" {
				st.Message += ": " + cs.State.Waiting.Message
			}
		} else if cs.State.Terminated != nil && cs.State.Terminated.Reason != "" && cs.State.Terminated.ExitCode != 0 {
			st.Message = cs.State.Terminated.Reason
		}
	}
	st.Ready = p.Status.Phase == corev1.PodRunning && ready
	if st.Ready {
		st.Status = "Ready"
	}
}

func phase(ready bool, desired, readyReps int32) string {
	if ready {
		return "Ready"
	}
	if desired == 0 {
		return "Complete"
	}
	if readyReps == 0 {
		return "Progressing"
	}
	return "Progressing"
}

func errReason(err error) string {
	if apierrors.IsNotFound(err) {
		return ""
	}
	return err.Error()
}

// ObjectLogs reads one object's container log. Only Pods are supported and the
// object MUST belong to the release's current revision (ownership gate).
func (c *Client) ObjectLogs(ctx context.Context, release, kind, name string, opts LogOptions) ([]string, bool, string, error) {
	if !strings.EqualFold(kind, "Pod") {
		return nil, false, "only Pod objects have container logs", nil
	}
	rel, ok, err := c.Get(ctx, release)
	if err != nil {
		return nil, false, "", err
	}
	if !ok {
		return nil, false, "", fmt.Errorf("release %q not found", release)
	}
	owned := false
	for _, o := range rel.Objects {
		if strings.EqualFold(o.Kind, "Pod") && o.Name == name {
			owned = true
			break
		}
	}
	if !owned {
		return nil, false, fmt.Sprintf("pod %q is not part of release %q", name, release), nil
	}
	p, err := c.cs.CoreV1().Pods(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, false, errReason(err), err
	}
	container := opts.Container
	if container == "" && len(p.Spec.Containers) > 0 {
		container = p.Spec.Containers[0].Name
	}
	tail := opts.TailLines
	if tail <= 0 {
		tail = 200
	}
	req := c.cs.CoreV1().Pods(c.namespace).GetLogs(name, &corev1.PodLogOptions{
		Container: container, Previous: opts.Previous, TailLines: &tail,
	})
	rc, err := req.Stream(ctx)
	if err != nil {
		return nil, false, err.Error(), nil
	}
	defer rc.Close()
	var lines []string
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil && err != io.EOF {
		return lines, len(lines) > 0, err.Error(), nil
	}
	msg := ""
	if len(lines) == 0 {
		var st ObjectStatus
		st.Kind, st.Name = "Pod", name
		applyPodStatus(&st, p)
		msg = fmt.Sprintf("no log output (phase=%s restarts=%d %s)", st.Phase, st.Restarts, st.Message)
	}
	return lines, len(lines) > 0, msg, nil
}

// LogOptions mirrors the service-log options for per-object reads.
type LogOptions struct {
	TailLines int64
	Previous  bool
	Container string
}
