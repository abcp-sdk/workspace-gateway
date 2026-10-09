package servicesmgr

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Revision is one rollout revision of a service's Deployment (backed by a
// ReplicaSet kept by the Deployment's revisionHistoryLimit).
type Revision struct {
	Revision int64
	Image    string
	Replicas int32
	Created  int64
	Current  bool
}

// revisionOf reads a Deployment/ReplicaSet's rollout revision annotation.
func revisionOf(annotations map[string]string) int64 {
	n, _ := strconv.ParseInt(annotations["deployment.kubernetes.io/revision"], 10, 64)
	return n
}

// Rollback rolls a service's Deployment back to a prior revision, mirroring
// `kubectl rollout undo`: it copies the target ReplicaSet's pod template onto
// the Deployment. revision == 0 undoes to the PREVIOUS revision; a positive
// revision rolls back TO that revision. How far back it can reach is bounded by
// the Deployment's revisionHistoryLimit (k8s default 10).
func (c *Client) Rollback(ctx context.Context, name string, revision int32) (Service, error) {
	d, err := c.cs.AppsV1().Deployments(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Service{}, err
	}
	target, err := c.targetRevision(ctx, d, revision)
	if err != nil {
		return Service{}, err
	}
	rs, err := c.cs.AppsV1().ReplicaSets(c.namespace).Get(ctx, target, metav1.GetOptions{})
	if err != nil {
		return Service{}, fmt.Errorf("revision %s not found: %w", target, err)
	}
	d.Spec.Template = *rs.Spec.Template.DeepCopy()
	if _, err := c.cs.AppsV1().Deployments(c.namespace).Update(ctx, d, metav1.UpdateOptions{}); err != nil {
		return Service{}, err
	}
	return c.Get(ctx, name)
}

// Revisions lists a service's rollout history, oldest first.
func (c *Client) Revisions(ctx context.Context, name string) ([]Revision, error) {
	d, err := c.cs.AppsV1().Deployments(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	cur := revisionOf(d.Annotations)
	rss, err := c.cs.AppsV1().ReplicaSets(c.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: metav1.FormatLabelSelector(d.Spec.Selector),
	})
	if err != nil {
		return nil, err
	}
	out := make([]Revision, 0, len(rss.Items))
	for i := range rss.Items {
		rs := &rss.Items[i]
		rev := revisionOf(rs.Annotations)
		if rev == 0 {
			continue
		}
		reps := int32(0)
		if rs.Spec.Replicas != nil {
			reps = *rs.Spec.Replicas
		}
		img := ""
		if len(rs.Spec.Template.Spec.Containers) > 0 {
			img = rs.Spec.Template.Spec.Containers[0].Image
		}
		out = append(out, Revision{
			Revision: rev, Image: img, Replicas: reps,
			Created: rs.CreationTimestamp.UnixMilli(), Current: rev == cur,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Revision < out[j].Revision })
	return out, nil
}

// targetRevision resolves the ReplicaSet name to roll back to.
func (c *Client) targetRevision(ctx context.Context, d *appsv1.Deployment, revision int32) (string, error) {
	rss, err := c.cs.AppsV1().ReplicaSets(c.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: metav1.FormatLabelSelector(d.Spec.Selector),
	})
	if err != nil {
		return "", err
	}
	type rr struct {
		name string
		rev  int64
	}
	revs := make([]rr, 0, len(rss.Items))
	for i := range rss.Items {
		rev := revisionOf(rss.Items[i].Annotations)
		if rev == 0 {
			continue
		}
		revs = append(revs, rr{rss.Items[i].Name, rev})
	}
	sort.Slice(revs, func(i, j int) bool { return revs[i].rev < revs[j].rev })
	if len(revs) == 0 {
		return "", fmt.Errorf("no rollout history")
	}
	if revision > 0 {
		for _, r := range revs {
			if r.rev == int64(revision) {
				return r.name, nil
			}
		}
		return "", fmt.Errorf("revision %d not found", revision)
	}
	cur := revisionOf(d.Annotations)
	for i := len(revs) - 1; i >= 0; i-- {
		if revs[i].rev < cur {
			return revs[i].name, nil
		}
	}
	return "", fmt.Errorf("no previous revision")
}
