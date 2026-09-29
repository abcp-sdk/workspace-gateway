package servicesmgr

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WaitResult is the outcome of waiting for a Deployment to become ready.
type WaitResult struct {
	// Ready is true when the Deployment has at least one ready replica.
	Ready bool
	// Failed is true on a DETERMINISTIC failure that will not resolve by
	// waiting (bad command, missing image, config error, unschedulable pod).
	Failed bool
	// Reason is a short human-readable explanation (k8s waiting/terminated
	// reason + message, or a scheduling failure). Empty when healthy.
	Reason string
}

// deterministicFailureReasons are container waiting reasons that will never
// resolve on their own — waiting longer is pointless, so the caller should
// surface them immediately instead of burning the whole timeout.
var deterministicFailureReasons = map[string]bool{
	"RunContainerError":          true,
	"CreateContainerError":       true,
	"CreateContainerConfigError": true,
	"InvalidImageName":           true,
	"ErrImagePull":               true,
	"ImagePullBackOff":           true,
	"ImageInspectError":          true,
	"ErrImageNeverPull":          true,
}

// WaitReady polls a service's Deployment (and its pods) until it is ready, a
// deterministic failure is observed, the timeout elapses, or ctx is done. It
// NEVER returns an error for "still not ready": the caller decides how to
// present a timeout. `deployName` is the concrete Deployment (for a blue-green
// slot, `<name>-green`).
func (c *Client) WaitReady(ctx context.Context, deployName string, timeout time.Duration) (WaitResult, error) {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		dep, err := c.cs.AppsV1().Deployments(c.namespace).Get(ctx, deployName, metav1.GetOptions{})
		if err != nil {
			return WaitResult{}, err
		}
		if ready, res := c.deploymentReadiness(ctx, dep); ready {
			return WaitResult{Ready: true}, nil
		} else if res.Failed {
			return res, nil
		}
		if !time.Now().Before(deadline) {
			// Timeout: report the latest observed reason (may be empty), but
			// this is NOT a hard failure.
			_, res := c.deploymentReadiness(ctx, dep)
			return res, nil
		}
		select {
		case <-ctx.Done():
			return WaitResult{}, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// deploymentReadiness inspects a Deployment's status + its pods: returns
// (ready, result). `result` carries a failure/explanation when not ready.
func (c *Client) deploymentReadiness(ctx context.Context, dep *appsv1.Deployment) (bool, WaitResult) {
	if dep.Status.ReadyReplicas > 0 {
		return true, WaitResult{Ready: true}
	}
	sel, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err != nil {
		return false, WaitResult{}
	}
	pods, err := c.cs.CoreV1().Pods(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil || len(pods.Items) == 0 {
		// No pod yet (scheduler/image not started). Keep waiting.
		return false, WaitResult{}
	}
	// Newest pod first.
	pod := pods.Items[0]
	for i := range pods.Items {
		if pods.Items[i].CreationTimestamp.After(pod.CreationTimestamp.Time) {
			pod = pods.Items[i]
		}
	}
	// A pod stuck Pending with an unschedulable condition is deterministic.
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse && cond.Reason == "Unschedulable" {
			return false, WaitResult{Failed: true, Reason: "FailedScheduling: " + cond.Message}
		}
	}
	// Container waiting/terminated reasons.
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting != nil {
			r := cs.State.Waiting.Reason
			if deterministicFailureReasons[r] {
				return false, WaitResult{Failed: true, Reason: waitingText(r, cs.State.Waiting.Message)}
			}
			if r != "" {
				return false, WaitResult{Reason: waitingText(r, cs.State.Waiting.Message)}
			}
		}
		if cs.State.Terminated != nil && cs.State.Terminated.Reason != "" && cs.State.Terminated.Reason != "Completed" {
			r := cs.State.Terminated.Reason
			return false, WaitResult{Reason: waitingText(r, cs.State.Terminated.Message)}
		}
	}
	return false, WaitResult{}
}

// waitingText joins a reason + message into one line.
func waitingText(reason, message string) string {
	if message == "" {
		return reason
	}
	if strings.HasPrefix(message, reason) {
		return message
	}
	return fmt.Sprintf("%s: %s", reason, message)
}
