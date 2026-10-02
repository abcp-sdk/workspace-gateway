// Package helmmgr renders a Helm chart from a repository (using the Helm SDK
// templating packages only) and applies the result to the managed namespace via
// a dynamic client, with a strict safety filter.
//
// Release state is split like upstream Helm's storage driver: a SMALL head
// ConfigMap per release (metadata + the list of stored revision numbers) and ONE
// ConfigMap PER revision (that revision's manifest + applied objects). Keeping
// every revision in a single object eventually exceeds the 1 MiB ConfigMap limit
// and then makes EVERY upgrade of that release fail, so revisions beyond
// `historyMax` are pruned after each write.
//
// It deliberately does NOT use Helm's action/installer stack: that pulls in
// kubectl/oras/kustomize and grants far more power than a tenant-scoped deploy
// surface should have. Templating is delegated to Helm; apply, ownership and
// rollback are owned here.
package helmmgr

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	sigsyaml "sigs.k8s.io/yaml"
)

// Allowed kinds a rendered manifest may contain. Anything else is refused, as
// are cluster-scoped resources, RBAC, CRDs and privileged pod features.
var allowedKinds = map[string]bool{
	"ConfigMap":               true,
	"Secret":                  true,
	"Service":                 true,
	"ServiceAccount":          true,
	"PersistentVolumeClaim":   true,
	"Deployment":              true,
	"StatefulSet":             true,
	"DaemonSet":               true,
	"Job":                     true,
	"CronJob":                 true,
	"Ingress":                 true,
	"NetworkPolicy":           true,
	"HorizontalPodAutoscaler": true,
	"PodDisruptionBudget":     true,
}

// Client renders + applies Helm charts in one namespace.
type Client struct {
	dyn       dynamic.Interface
	cs        kubernetes.Interface
	mapper    meta.RESTMapper
	namespace string
	// historyMax caps the revisions kept per release; <=0 means unlimited.
	historyMax int
}

// Config configures a Client.
type Config struct {
	Namespace string
	// HistoryMax caps the number of revisions kept per release (older ones are
	// pruned after each write). 0 = the package default (defaultHistoryMax);
	// negative = keep every revision (unbounded).
	HistoryMax int
}

// New builds a client from in-cluster config.
func New(cfg Config) (*Client, error) {
	rc, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("helmmgr: in-cluster config: %w", err)
	}
	return NewForConfig(rc, cfg)
}

// NewForConfig builds a client from an explicit rest config.
func NewForConfig(rc *rest.Config, cfg Config) (*Client, error) {
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	dc, err := discovery.NewDiscoveryClientForConfig(rc)
	if err != nil {
		return nil, err
	}
	gr, err := restmapper.GetAPIGroupResources(dc)
	if err != nil {
		return nil, fmt.Errorf("helmmgr: discovery: %w", err)
	}
	ns := cfg.Namespace
	if ns == "" {
		ns = "worker"
	}
	hm := cfg.HistoryMax
	if hm == 0 {
		hm = defaultHistoryMax
	}
	return &Client{dyn: dyn, cs: cs, mapper: restmapper.NewDiscoveryRESTMapper(gr), namespace: ns, historyMax: hm}, nil
}

// Object is one rendered + applied resource.
type Object struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
}

// Revision is one stored release revision.
type Revision struct {
	Revision  int    `json:"revision"`
	Ref       string `json:"ref"`
	ChartPath string `json:"chartPath"`
	// ChartVersion/AppVersion come from the chart's Chart.yaml at this revision.
	ChartVersion string   `json:"chartVersion,omitempty"`
	AppVersion   string   `json:"appVersion,omitempty"`
	Values       string   `json:"values"`
	CreatedAt    int64    `json:"createdAt"`
	Objects      []Object `json:"objects"`
	Manifest     string   `json:"manifest"`
}

// Release is a Helm release managed by the gateway.
type Release struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Creator   string `json:"creator"`
	Session   string `json:"session"`
	Ref       string `json:"ref"`
	ChartPath string `json:"chartPath"`
	// ChartVersion/AppVersion are the CURRENT revision's chart metadata
	// (Chart.yaml version / appVersion). Denormalized onto the head so a list
	// view can show them without reading the revision bodies.
	ChartVersion string     `json:"chartVersion,omitempty"`
	AppVersion   string     `json:"appVersion,omitempty"`
	Revision     int        `json:"revision"`
	Status       string     `json:"status"`
	UpdatedAt    int64      `json:"updatedAt"`
	History      []Revision `json:"history,omitempty"`
	// Objects are the CURRENT revision's applied objects ("Kind/name" lives in
	// the proto). Small (tens of entries), so it stays in the head record.
	Objects []Object `json:"objects,omitempty"`
	// Slot is "blue"/"green" for a slot release ("" = plain).
	Slot string `json:"slot,omitempty"`
	// Router is the logical release name a slot release belongs to (empty for a
	// plain release). The router Service is named `Router`; its selector points
	// at the ACTIVE slot's pods.
	Router string `json:"router,omitempty"`
	// ActiveSlot is set on the ROUTER record (Router == Name) to record which
	// slot the router currently selects.
	ActiveSlot string `json:"activeSlot,omitempty"`
}

const (
	releaseLabel  = "workspace/helm-release"
	releaseCMName = "helm-release-"
	// revisionCMName prefixes a per-revision body ConfigMap
	// (`helm-release-rev-<release>-v<N>`).
	revisionCMName  = "helm-release-rev-"
	annoReleaseName = "workspace/helm-release-name"
	// recordKindLabel distinguishes a release HEAD record from a REVISION body
	// (both carry releaseLabel, so ONE watch covers the whole release).
	recordKindLabel = "workspace/helm-record"
	recordHead      = "head"
	recordRevision  = "revision"
	// revisionLabel carries the revision NUMBER on a revision body.
	revisionLabel = "workspace/helm-revision"
	// defaultHistoryMax is the default per-release revision cap (Config.HistoryMax
	// 0 selects it). Bounded so release state can never approach the 1 MiB
	// ConfigMap limit that a single all-revisions object eventually hits.
	defaultHistoryMax = 10
	// slotLabel is injected into every workload pod template of a slot release,
	// so the router Service can select the ACTIVE slot's pods.
	slotLabel = "workspace/helm-slot"
	// routerAnno marks the router Service of a blue-green Helm release.
	routerAnno = "workspace/helm-router"
	// routerActiveAnno records which slot the router currently selects.
	routerActiveAnno = "workspace/helm-router-active"
)

// Slot names for a blue-green Helm release (mirrors servicesmgr).
const (
	SlotBlue  = "blue"
	SlotGreen = "green"
)

// ValidHelmSlot reports whether s names a blue-green slot ("" = plain release).
func ValidHelmSlot(s string) bool { return s == "" || s == SlotBlue || s == SlotGreen }

// RenderOptions describes one render+apply request.
type RenderOptions struct {
	// Release name (DNS-1123 label).
	Release string
	// ChartPath is the repo-relative directory holding Chart.yaml.
	ChartPath string
	// ValuesYAML is the user-supplied values document (may be empty).
	ValuesYAML string
	// Ref is the git ref the chart came from (recorded for history).
	Ref string
	// Slot is "blue"/"green" for a blue-green release ("" = plain). A slot
	// release is stored under `<release>-<slot>` and its pods carry the slot
	// label so a router Service can switch between slots.
	Slot string
}

// Template renders the chart at chartDir (an extracted repo root + ChartPath)
// into a filtered, namespace-pinned manifest. It returns the rendered docs, the
// applied objects (dry-run = no apply), and the chart's metadata (Chart.yaml
// version / appVersion).
func (c *Client) Template(chartDir string, o RenderOptions) (string, []Object, ChartMeta, error) {
	ch, err := loader.LoadDir(chartDir)
	if err != nil {
		return "", nil, ChartMeta{}, fmt.Errorf("load chart: %w", err)
	}
	meta := ChartMeta{}
	if ch.Metadata != nil {
		meta.Version = ch.Metadata.Version
		meta.AppVersion = ch.Metadata.AppVersion
	}
	vals := map[string]any{}
	if strings.TrimSpace(o.ValuesYAML) != "" {
		if err := yaml.Unmarshal([]byte(o.ValuesYAML), &vals); err != nil {
			return "", nil, ChartMeta{}, fmt.Errorf("parse values: %w", err)
		}
	}
	relOpts := chartutil.ReleaseOptions{Name: o.Release, Namespace: c.namespace, Revision: 1, IsInstall: true}
	caps := chartutil.DefaultCapabilities
	renderVals, err := chartutil.ToRenderValues(ch, vals, relOpts, caps)
	if err != nil {
		return "", nil, ChartMeta{}, fmt.Errorf("render values: %w", err)
	}
	rendered, err := engine.Render(ch, renderVals)
	if err != nil {
		return "", nil, ChartMeta{}, fmt.Errorf("render: %w", err)
	}
	// Collect the rendered templates in a deterministic order.
	names := make([]string, 0, len(rendered))
	for n := range rendered {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	first := true
	for _, n := range names {
		if strings.HasSuffix(n, "_helpers.tpl") || strings.HasSuffix(n, "NOTES.txt") {
			continue
		}
		content := strings.TrimSpace(rendered[n])
		if content == "" {
			continue
		}
		if !first {
			b.WriteString("\n---\n")
		}
		b.WriteString(content)
		first = false
	}
	manifest := b.String()
	objects, err := c.validate(manifest)
	if err != nil {
		return "", nil, ChartMeta{}, err
	}
	return manifest, objects, meta, nil
}

// ChartMeta is the chart identity read from Chart.yaml.
type ChartMeta struct {
	Version    string
	AppVersion string
}

// workloadPodSpecs are the unstructured paths to a workload's pod template
// "spec" (where labels + selector live).
var workloadPodSpecs = map[string][][]string{
	"Deployment":  {{"spec", "template", "spec"}},
	"StatefulSet": {{"spec", "template", "spec"}},
	"DaemonSet":   {{"spec", "template", "spec"}},
	"Job":         {{"spec", "template", "spec"}},
	"CronJob":     {{"spec", "jobTemplate", "spec", "template", "spec"}},
}

// workloadPodLabels are the matching pod-template LABELS paths (so a Service
// selector can match the pods).
var workloadPodLabels = map[string][][]string{
	"Deployment":  {{"spec", "template", "metadata", "labels"}},
	"StatefulSet": {{"spec", "template", "metadata", "labels"}},
	"DaemonSet":   {{"spec", "template", "metadata", "labels"}},
	"Job":         {{"spec", "template", "metadata", "labels"}},
	"CronJob":     {{"spec", "jobTemplate", "spec", "template", "metadata", "labels"}},
}

// validate parses every non-empty YAML doc, enforces the kind whitelist and
// namespace, strips dangerous fields, and returns the object identities.
func (c *Client) validate(manifest string) ([]Object, error) {
	var out []Object
	docs := strings.Split(manifest, "\n---")
	for _, doc := range docs {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var u unstructured.Unstructured
		if err := yaml.Unmarshal([]byte(doc), &u.Object); err != nil {
			return nil, fmt.Errorf("parse rendered doc: %w", err)
		}
		if len(u.Object) == 0 {
			continue
		}
		kind := u.GetKind()
		if kind == "" {
			continue
		}
		if !allowedKinds[kind] {
			return nil, fmt.Errorf("kind %q is not allowed", kind)
		}
		// No cluster-scoped objects: a namespaced kind must set/accept a namespace.
		ns := u.GetNamespace()
		if ns != "" && ns != c.namespace {
			return nil, fmt.Errorf("%s/%s: namespace %q is not allowed (only %q)", kind, u.GetName(), ns, c.namespace)
		}
		if isClusterScoped(c.mapper, u.GroupVersionKind()) {
			return nil, fmt.Errorf("%s is cluster-scoped and not allowed", kind)
		}
		if err := forbidDangerous(&u); err != nil {
			return nil, fmt.Errorf("%s/%s: %w", kind, u.GetName(), err)
		}
		out = append(out, Object{APIVersion: u.GetAPIVersion(), Kind: kind, Name: u.GetName()})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("chart rendered no objects")
	}
	return out, nil
}

// forbidDangerous rejects privileged/host-access pod features anywhere in the doc.
func forbidDangerous(u *unstructured.Unstructured) error {
	bad := []struct {
		path []string
		want any
		why  string
	}{
		{[]string{"spec", "template", "spec", "hostNetwork"}, true, "hostNetwork"},
		{[]string{"spec", "template", "spec", "hostPID"}, true, "hostPID"},
		{[]string{"spec", "template", "spec", "hostIPC"}, true, "hostIPC"},
	}
	for _, b := range bad {
		if v, found, _ := unstructured.NestedFieldNoCopy(u.Object, b.path...); found && v == b.want {
			return fmt.Errorf("%s is not allowed", b.why)
		}
	}
	// Reject hostPath volumes and privileged containers in the pod template.
	for _, base := range [][]string{{"spec", "template", "spec"}, {"spec", "jobTemplate", "spec", "template", "spec"}} {
		vols, _, _ := unstructured.NestedSlice(u.Object, append(append([]string{}, base...), "volumes")...)
		for _, v := range vols {
			if m, ok := v.(map[string]any); ok {
				if _, has := m["hostPath"]; has {
					return fmt.Errorf("hostPath volumes are not allowed")
				}
			}
		}
		containers, _, _ := unstructured.NestedSlice(u.Object, append(append([]string{}, base...), "containers")...)
		for _, cn := range containers {
			if m, ok := cn.(map[string]any); ok {
				if sc, ok := m["securityContext"].(map[string]any); ok {
					if p, _ := sc["privileged"].(bool); p {
						return fmt.Errorf("privileged containers are not allowed")
					}
				}
			}
		}
	}
	return nil
}

func isClusterScoped(mapper meta.RESTMapper, gvk schema.GroupVersionKind) bool {
	mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return false
	}
	return mapping.Scope.Name() == meta.RESTScopeNameRoot
}

// ---- release state (ConfigMap-backed, one object per revision) ----
//
// A release's state lives in TWO kinds of ConfigMap:
//   - the HEAD record `helm-release-<release>`: release metadata + the revision
//     numbers currently stored (the history itself is materialized on read);
//   - one BODY per revision `helm-release-rev-<release>-v<N>`: that revision's
//     ref/chart/values/objects/manifest.
//
// Both carry releaseLabel so a single watch still covers the whole release;
// recordKindLabel tells them apart. At most `historyMax` revisions are kept.
// This mirrors upstream Helm's storage driver: a single all-revisions object
// eventually exceeds the 1 MiB ConfigMap limit and then makes EVERY upgrade of
// that release fail.

func (c *Client) cmName(release string) string { return releaseCMName + release }

// revCMName is the ConfigMap name holding one revision's body.
func (c *Client) revCMName(release string, revision int) string {
	return revisionCMName + release + "-v" + strconv.Itoa(revision)
}

// Get returns a release (its history materialized from the revision bodies), or
// ok=false when absent.
func (c *Client) Get(ctx context.Context, release string) (Release, bool, error) {
	head, err := c.cs.CoreV1().ConfigMaps(c.namespace).Get(ctx, c.cmName(release), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Release{}, false, nil
	}
	if err != nil {
		return Release{}, false, err
	}
	var r Release
	if err := json.Unmarshal([]byte(head.Data["release"]), &r); err != nil {
		return Release{}, false, err
	}
	if len(r.History) > 0 {
		// LEGACY layout: every revision lived inside the head's "release" blob.
		// Migrate it (best-effort — the read still succeeds even if the rewrite
		// fails, and a later write migrates it for real).
		_ = c.migrateLegacy(ctx, r)
	}
	revs, err := c.loadRevisions(ctx, release)
	if err != nil {
		return Release{}, false, err
	}
	r.History = revs
	return r, true, nil
}

// migrateLegacy rewrites a pre-split release (whose every revision lived inside
// the head blob) as one revision body per revision + a metadata-only head.
func (c *Client) migrateLegacy(ctx context.Context, r Release) error {
	for _, rv := range r.History {
		if err := c.putRevision(ctx, r.Name, rv); err != nil {
			return err
		}
	}
	return c.put(ctx, r)
}

// loadRevisions reads the release's revision bodies, oldest first.
func (c *Client) loadRevisions(ctx context.Context, release string) ([]Revision, error) {
	list, err := c.cs.CoreV1().ConfigMaps(c.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: releaseLabel + "=" + release + "," + recordKindLabel + "=" + recordRevision,
	})
	if err != nil {
		return nil, err
	}
	out := make([]Revision, 0, len(list.Items))
	for i := range list.Items {
		var rv Revision
		if err := json.Unmarshal([]byte(list.Items[i].Data["revision"]), &rv); err != nil {
			continue
		}
		out = append(out, rv)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Revision < out[j].Revision })
	return out, nil
}

// put writes the head record and prunes revisions beyond historyMax.
func (c *Client) put(ctx context.Context, r Release) error {
	if err := c.putHead(ctx, r); err != nil {
		return err
	}
	return c.pruneHistory(ctx, r)
}

// putHead upserts the release's head record (metadata + stored revision numbers).
// The head carries NO manifests, so it stays tiny no matter how many revisions a
// release accumulates.
func (c *Client) putHead(ctx context.Context, r Release) error {
	nums := make([]int, 0, len(r.History))
	for _, rv := range r.History {
		nums = append(nums, rv.Revision)
	}
	head := r
	head.History = nil
	b, err := json.Marshal(head)
	if err != nil {
		return err
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: c.cmName(r.Name), Namespace: c.namespace,
			Labels: map[string]string{
				releaseLabel: r.Name, recordKindLabel: recordHead,
				"app.kubernetes.io/managed-by": "workspace-gateway",
			},
		},
		Data: map[string]string{"release": string(b), "revisions": joinInts(nums)},
	}
	if _, err := c.cs.CoreV1().ConfigMaps(c.namespace).Create(ctx, cm, metav1.CreateOptions{}); err == nil {
		return nil
	} else if !apierrors.IsAlreadyExists(err) {
		return err
	}
	cur, err := c.cs.CoreV1().ConfigMaps(c.namespace).Get(ctx, cm.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	cm.ResourceVersion = cur.ResourceVersion
	_, err = c.cs.CoreV1().ConfigMaps(c.namespace).Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

// putRevision stores one revision's body in its own ConfigMap.
func (c *Client) putRevision(ctx context.Context, release string, rv Revision) error {
	b, err := json.Marshal(rv)
	if err != nil {
		return err
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: c.revCMName(release, rv.Revision), Namespace: c.namespace,
			Labels: map[string]string{
				releaseLabel: release, recordKindLabel: recordRevision,
				revisionLabel:                  strconv.Itoa(rv.Revision),
				"app.kubernetes.io/managed-by": "workspace-gateway",
			},
		},
		Data: map[string]string{"revision": string(b)},
	}
	if _, err := c.cs.CoreV1().ConfigMaps(c.namespace).Create(ctx, cm, metav1.CreateOptions{}); err == nil {
		return nil
	} else if !apierrors.IsAlreadyExists(err) {
		return err
	}
	cur, err := c.cs.CoreV1().ConfigMaps(c.namespace).Get(ctx, cm.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	cm.ResourceVersion = cur.ResourceVersion
	_, err = c.cs.CoreV1().ConfigMaps(c.namespace).Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

// pruneHistory deletes the revision bodies beyond the newest historyMax, so a
// release's stored state is bounded. A no-op when historyMax <= 0.
func (c *Client) pruneHistory(ctx context.Context, r Release) error {
	if c.historyMax <= 0 {
		return nil
	}
	nums := make([]int, 0, len(r.History))
	for _, rv := range r.History {
		nums = append(nums, rv.Revision)
	}
	sort.Ints(nums)
	keep := map[int]bool{}
	for i, n := range nums {
		if i < len(nums)-c.historyMax {
			continue
		}
		keep[n] = true
	}
	stored, err := c.cs.CoreV1().ConfigMaps(c.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: releaseLabel + "=" + r.Name + "," + recordKindLabel + "=" + recordRevision,
	})
	if err != nil {
		return err
	}
	for i := range stored.Items {
		n, err := strconv.Atoi(stored.Items[i].Labels[revisionLabel])
		if err != nil || keep[n] {
			continue
		}
		if err := c.cs.CoreV1().ConfigMaps(c.namespace).Delete(ctx, stored.Items[i].Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// deleteReleaseState removes a release's head record and every revision body.
func (c *Client) deleteReleaseState(ctx context.Context, release string) error {
	stored, err := c.cs.CoreV1().ConfigMaps(c.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: releaseLabel + "=" + release,
	})
	if err != nil {
		return err
	}
	for i := range stored.Items {
		if err := c.cs.CoreV1().ConfigMaps(c.namespace).Delete(ctx, stored.Items[i].Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	if err := c.cs.CoreV1().ConfigMaps(c.namespace).Delete(ctx, c.cmName(release), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// joinInts renders revision numbers as a comma-separated list (head metadata for
// operators; the authoritative list is the revision bodies themselves).
func joinInts(ns []int) string {
	parts := make([]string, 0, len(ns))
	for _, n := range ns {
		parts = append(parts, strconv.Itoa(n))
	}
	return strings.Join(parts, ",")
}

// recordRevision persists a release's NEWEST revision body and then its head
// record (which prunes revisions beyond historyMax). It is the write path shared
// by Apply/ApplySlot/Rollback.
func (c *Client) recordRevision(ctx context.Context, r Release) (Release, error) {
	if len(r.History) == 0 {
		return Release{}, fmt.Errorf("release %q has no revision to record", r.Name)
	}
	if err := c.putRevision(ctx, r.Name, r.History[len(r.History)-1]); err != nil {
		return Release{}, err
	}
	if err := c.put(ctx, r); err != nil {
		return Release{}, err
	}
	return r, nil
}

// List returns every release in the namespace (newest first). Revision bodies
// are skipped (they carry recordRevision): the head carries the metadata, and
// history is loaded on demand. A legacy head (written before the split, without
// recordKindLabel) is still listed and migrated by the next Get/Apply.
func (c *Client) List(ctx context.Context) ([]Release, error) {
	cms, err := c.cs.CoreV1().ConfigMaps(c.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: releaseLabel,
	})
	if err != nil {
		return nil, err
	}
	out := make([]Release, 0, len(cms.Items))
	for i := range cms.Items {
		if cms.Items[i].Labels[recordKindLabel] == recordRevision {
			continue
		}
		var r Release
		if err := json.Unmarshal([]byte(cms.Items[i].Data["release"]), &r); err != nil {
			continue
		}
		r.History = nil
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	return out, nil
}

// Watch streams changes to release-state ConfigMaps (create/update/delete), so
// the gateway's WatchWorkspace can push a fresh Helm-release list live.
func (c *Client) Watch(ctx context.Context) (watch.Interface, error) {
	return c.cs.CoreV1().ConfigMaps(c.namespace).Watch(ctx, metav1.ListOptions{
		LabelSelector: releaseLabel,
	})
}

// ---- apply ----

// Apply renders + applies a chart as a NEW or updated release. It records a
// new revision in the release history.
func (c *Client) Apply(ctx context.Context, chartDir string, o RenderOptions, creator, session string) (Release, error) {
	if !validReleaseName(o.Release) {
		return Release{}, fmt.Errorf("release name must be a DNS-1123 label")
	}
	manifest, objects, meta, err := c.Template(chartDir, o)
	if err != nil {
		return Release{}, err
	}
	if err := c.applyManifest(ctx, manifest, o.Release); err != nil {
		return Release{}, err
	}
	cur, ok, err := c.Get(ctx, o.Release)
	if err != nil {
		return Release{}, err
	}
	if !ok {
		cur = Release{Name: o.Release, Namespace: c.namespace, Creator: creator, Session: session}
	}
	cur.Ref = o.Ref
	cur.ChartPath = o.ChartPath
	cur.ChartVersion = meta.Version
	cur.AppVersion = meta.AppVersion
	cur.Objects = objects
	cur.Revision++
	cur.Status = "deployed"
	cur.UpdatedAt = time.Now().UnixMilli()
	cur.History = append(cur.History, Revision{
		Revision: cur.Revision, Ref: o.Ref, ChartPath: o.ChartPath, Values: o.ValuesYAML,
		ChartVersion: meta.Version, AppVersion: meta.AppVersion,
		CreatedAt: cur.UpdatedAt, Objects: objects, Manifest: manifest,
	})
	if _, err := c.recordRevision(ctx, cur); err != nil {
		return Release{}, err
	}
	return cur, nil
}

// ---- blue-green slots ----

// slotReleaseName is the stored release name for a slot: `<release>-<slot>`.
func slotReleaseName(release, slot string) string { return release + "-" + slot }

// ApplySlot renders + applies a chart as a SLOT release and manages the router
// Service so the primary URL targets the ACTIVE slot. The router is created on
// the first slot deploy (activating that slot); a green deploy does NOT
// auto-promote (the router stays on blue).
func (c *Client) ApplySlot(ctx context.Context, chartDir string, o RenderOptions, creator, session string) (Release, error) {
	if !ValidHelmSlot(o.Slot) || o.Slot == "" {
		return Release{}, fmt.Errorf("slot must be blue|green")
	}
	router := o.Release
	stored := slotReleaseName(router, o.Slot)
	// Render with the STORED release name so `.Release.Name` differs per slot
	// and the two slots' objects never collide.
	renderOpts := o
	renderOpts.Release = stored
	manifest, objects, meta, err := c.Template(chartDir, renderOpts)
	if err != nil {
		return Release{}, err
	}
	// Inject the release + slot labels into every workload pod template so the
	// router Service (selector release=<slotRelease>, slot=<active>) can target
	// this slot's pods.
	manifest, err = injectSlotLabel(manifest, stored, o.Slot)
	if err != nil {
		return Release{}, err
	}
	if err := c.applyManifest(ctx, manifest, stored); err != nil {
		return Release{}, err
	}
	// Ensure the router Service exists (activate the slot just deployed if new).
	if err := c.ensureRouter(ctx, router, o.Slot, manifest, creator, session); err != nil {
		return Release{}, err
	}
	cur, ok, err := c.Get(ctx, stored)
	if err != nil {
		return Release{}, err
	}
	if !ok {
		cur = Release{Name: stored, Namespace: c.namespace, Creator: creator, Session: session, Slot: o.Slot, Router: router}
	}
	cur.Ref = o.Ref
	cur.ChartPath = o.ChartPath
	cur.ChartVersion = meta.Version
	cur.AppVersion = meta.AppVersion
	cur.Objects = objects
	cur.Slot = o.Slot
	cur.Router = router
	cur.Revision++
	cur.Status = "deployed"
	cur.UpdatedAt = time.Now().UnixMilli()
	cur.History = append(cur.History, Revision{
		Revision: cur.Revision, Ref: o.Ref, ChartPath: o.ChartPath, Values: o.ValuesYAML,
		ChartVersion: meta.Version, AppVersion: meta.AppVersion,
		CreatedAt: cur.UpdatedAt, Objects: objects, Manifest: manifest,
	})
	if _, err := c.recordRevision(ctx, cur); err != nil {
		return Release{}, err
	}
	return cur, nil
}

// Promote switches the router Service to the READY non-active slot.
func (c *Client) Promote(ctx context.Context, router string, requireReady bool) (Release, error) {
	active, has, err := c.routerActive(ctx, router)
	if err != nil {
		return Release{}, err
	}
	if !has {
		return Release{}, fmt.Errorf("release %q is not blue-green (no router)", router)
	}
	target := SlotGreen
	if active == SlotGreen {
		target = SlotBlue
	}
	if _, ok, gerr := c.Get(ctx, slotReleaseName(router, target)); gerr != nil {
		return Release{}, gerr
	} else if !ok {
		return Release{}, fmt.Errorf("slot %q does not exist", target)
	}
	if requireReady {
		ready, rerr := c.slotReady(ctx, slotReleaseName(router, target))
		if rerr != nil {
			return Release{}, rerr
		}
		if !ready {
			return Release{}, fmt.Errorf("slot %q is not ready", target)
		}
	}
	return c.setRouterActive(ctx, router, target)
}

// Rollback switches the router back to the other slot (inverse of Promote).
func (c *Client) RollbackRouter(ctx context.Context, router string) (Release, error) {
	active, has, err := c.routerActive(ctx, router)
	if err != nil {
		return Release{}, err
	}
	if !has {
		return Release{}, fmt.Errorf("release %q is not blue-green (no router)", router)
	}
	target := SlotGreen
	if active == SlotGreen {
		target = SlotBlue
	}
	if _, ok, gerr := c.Get(ctx, slotReleaseName(router, target)); gerr != nil {
		return Release{}, gerr
	} else if !ok {
		return Release{}, fmt.Errorf("slot %q does not exist", target)
	}
	return c.setRouterActive(ctx, router, target)
}

// SlotInfo is one slot's live status.
type SlotInfo struct {
	Slot          string `json:"slot"`
	Release       string `json:"release"`
	Ready         bool   `json:"ready"`
	ReadyWorkload int    `json:"readyWorkload"`
	TotalWorkload int    `json:"totalWorkload"`
}

// Slots returns the live slots of a blue-green release + the active slot.
func (c *Client) Slots(ctx context.Context, router string) ([]SlotInfo, string, error) {
	active, has, err := c.routerActive(ctx, router)
	if err != nil || !has {
		return nil, "", err
	}
	var out []SlotInfo
	for _, slot := range []string{SlotBlue, SlotGreen} {
		name := slotReleaseName(router, slot)
		_, ok, gerr := c.Get(ctx, name)
		if gerr != nil {
			return nil, "", gerr
		}
		if !ok {
			continue
		}
		readyN, totalN, rerr := c.slotWorkloadReady(ctx, name)
		if rerr != nil {
			return nil, "", rerr
		}
		out = append(out, SlotInfo{Slot: slot, Release: name, Ready: totalN > 0 && readyN == totalN, ReadyWorkload: readyN, TotalWorkload: totalN})
	}
	return out, active, nil
}

// UninstallRouter removes a blue-green release: both slots, the router Service,
// and the release records.
func (c *Client) UninstallRouter(ctx context.Context, router string) (bool, error) {
	any := false
	for _, slot := range []string{SlotBlue, SlotGreen} {
		if ok, err := c.Uninstall(ctx, slotReleaseName(router, slot)); err != nil {
			return false, err
		} else if ok {
			any = true
		}
	}
	// Delete the router Service + its record.
	if err := c.deleteRouterService(ctx, router); err != nil {
		return false, err
	}
	if _, ok, _ := c.Get(ctx, router); ok {
		_ = c.deleteReleaseState(ctx, router)
		any = true
	}
	return any, nil
}

// ---- router internals ----

// routerServiceName is the router Service name (the logical release name).
func (c *Client) routerServiceName(router string) string { return router }

// ensureRouter creates the router Service if absent. The FIRST slot deployed
// activates that slot; an existing router is left on its current active slot.
func (c *Client) ensureRouter(ctx context.Context, router, slot, manifest, creator, session string) error {
	// Reuse the first Service in the slot manifest for ports (the chart's own
	// Service). If none, derive from the first workload's container port.
	ports := routerPortsFromManifest(manifest)
	if len(ports) == 0 {
		return fmt.Errorf("blue-green requires the chart to render a Service (or a containerPort)")
	}
	cur, err := c.cs.CoreV1().Services(c.namespace).Get(ctx, c.routerServiceName(router), metav1.GetOptions{})
	if err == nil {
		// Existing router: keep its active slot (do not auto-promote).
		if cur.Annotations[routerActiveAnno] == "" {
			// Adopt as blue.
			if _, err := c.setRouterActive(ctx, router, SlotBlue); err != nil {
				return err
			}
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	active := slot
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: c.routerServiceName(router), Namespace: c.namespace,
			Labels: map[string]string{
				releaseLabel: router, routerAnno: "1",
				"app.kubernetes.io/managed-by": "workspace-gateway",
			},
			Annotations: map[string]string{routerActiveAnno: active},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{releaseLabel: slotReleaseName(router, active), slotLabel: active},
			Ports:    ports,
		},
	}
	if _, err := c.cs.CoreV1().Services(c.namespace).Create(ctx, svc, metav1.CreateOptions{}); err != nil {
		return err
	}
	// Record a router entry so List/History can surface it.
	rec := Release{
		Name: router, Namespace: c.namespace, Creator: creator, Session: session,
		ActiveSlot: active, Status: "deployed", UpdatedAt: time.Now().UnixMilli(),
	}
	return c.put(ctx, rec)
}

// routerActive reads the router Service's active-slot annotation.
func (c *Client) routerActive(ctx context.Context, router string) (string, bool, error) {
	svc, err := c.cs.CoreV1().Services(c.namespace).Get(ctx, c.routerServiceName(router), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	a := svc.Annotations[routerActiveAnno]
	return a, a != "", nil
}

// setRouterActive repoints the router Service at `slot` and records it.
func (c *Client) setRouterActive(ctx context.Context, router, slot string) (Release, error) {
	svc, err := c.cs.CoreV1().Services(c.namespace).Get(ctx, c.routerServiceName(router), metav1.GetOptions{})
	if err != nil {
		return Release{}, err
	}
	svc.Spec.Selector = map[string]string{releaseLabel: slotReleaseName(router, slot), slotLabel: slot}
	if svc.Annotations == nil {
		svc.Annotations = map[string]string{}
	}
	svc.Annotations[routerActiveAnno] = slot
	if _, err := c.cs.CoreV1().Services(c.namespace).Update(ctx, svc, metav1.UpdateOptions{}); err != nil {
		return Release{}, err
	}
	rec, ok, err := c.Get(ctx, router)
	if err != nil {
		return Release{}, err
	}
	if !ok {
		rec = Release{Name: router, Namespace: c.namespace}
	}
	rec.ActiveSlot = slot
	rec.Status = "deployed"
	rec.UpdatedAt = time.Now().UnixMilli()
	if err := c.put(ctx, rec); err != nil {
		return Release{}, err
	}
	return rec, nil
}

func (c *Client) deleteRouterService(ctx context.Context, router string) error {
	err := c.cs.CoreV1().Services(c.namespace).Delete(ctx, c.routerServiceName(router), metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// slotWorkloadReady counts ready/total workload objects of a slot release.
func (c *Client) slotWorkloadReady(ctx context.Context, release string) (int, int, error) {
	deps, err := c.cs.AppsV1().Deployments(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: releaseLabel + "=" + release})
	if err != nil {
		return 0, 0, err
	}
	ready, total := 0, 0
	for i := range deps.Items {
		d := &deps.Items[i]
		total++
		reps := int32(0)
		if d.Spec.Replicas != nil {
			reps = *d.Spec.Replicas
		}
		if reps > 0 && d.Status.ReadyReplicas >= reps {
			ready++
		}
	}
	// Also count StatefulSets.
	stss, _ := c.cs.AppsV1().StatefulSets(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: releaseLabel + "=" + release})
	for i := range stss.Items {
		s := &stss.Items[i]
		total++
		reps := int32(0)
		if s.Spec.Replicas != nil {
			reps = *s.Spec.Replicas
		}
		if reps > 0 && s.Status.ReadyReplicas >= reps {
			ready++
		}
	}
	return ready, total, nil
}

func (c *Client) slotReady(ctx context.Context, release string) (bool, error) {
	ready, total, err := c.slotWorkloadReady(ctx, release)
	if err != nil {
		return false, err
	}
	return total > 0 && ready == total, nil
}

// injectSlotLabel parses the manifest and adds the release + slot labels to
// every workload pod template so the router Service can select this slot's pods.
func injectSlotLabel(manifest, release, slot string) (string, error) {
	var out []string
	for _, doc := range strings.Split(manifest, "\n---") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var u unstructured.Unstructured
		if err := yaml.Unmarshal([]byte(doc), &u.Object); err != nil {
			return "", err
		}
		if len(u.Object) == 0 || u.GetKind() == "" {
			out = append(out, doc)
			continue
		}
		if metas, ok := workloadPodLabels[u.GetKind()]; ok {
			for _, meta := range metas {
				labels, _, _ := unstructured.NestedStringMap(u.Object, meta...)
				if labels == nil {
					labels = map[string]string{}
				}
				labels[releaseLabel] = release
				labels[slotLabel] = slot
				_ = unstructured.SetNestedStringMap(u.Object, labels, meta...)
			}
		}
		b, err := sigsyaml.Marshal(u.Object)
		if err != nil {
			return "", err
		}
		out = append(out, strings.TrimRight(string(b), "\n"))
	}
	return strings.Join(out, "\n---\n"), nil
}

// routerPortsFromManifest extracts the first Service's ports; if the chart
// renders no Service, derive a single tcp port from the first containerPort.
func routerPortsFromManifest(manifest string) []corev1.ServicePort {
	for _, doc := range strings.Split(manifest, "\n---") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var u unstructured.Unstructured
		if err := yaml.Unmarshal([]byte(doc), &u.Object); err != nil {
			continue
		}
		if u.GetKind() != "Service" {
			continue
		}
		var ports []corev1.ServicePort
		raw, _, _ := unstructured.NestedSlice(u.Object, "spec", "ports")
		for _, p := range raw {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			port := toInt32(m["port"])
			target := m["targetPort"]
			if port == 0 {
				continue
			}
			sp := corev1.ServicePort{Name: "http", Port: port, Protocol: corev1.ProtocolTCP}
			switch t := target.(type) {
			case int64:
				sp.TargetPort = intstr.FromInt32(int32(t))
			case int:
				sp.TargetPort = intstr.FromInt32(int32(t))
			case float64:
				sp.TargetPort = intstr.FromInt32(int32(t))
			case string:
				sp.TargetPort = intstr.FromString(t)
			default:
				sp.TargetPort = intstr.FromInt32(port)
			}
			ports = append(ports, sp)
		}
		if len(ports) > 0 {
			return ports
		}
	}
	// No Service: derive from the first containerPort.
	for _, doc := range strings.Split(manifest, "\n---") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var u unstructured.Unstructured
		if err := yaml.Unmarshal([]byte(doc), &u.Object); err != nil {
			continue
		}
		if _, ok := workloadPodSpecs[u.GetKind()]; !ok {
			continue
		}
		specs := workloadPodSpecs[u.GetKind()]
		containers, _, _ := unstructured.NestedSlice(u.Object, append(append([]string{}, specs[0]...), "containers")...)
		for _, cn := range containers {
			m, ok := cn.(map[string]any)
			if !ok {
				continue
			}
			ports, _ := m["ports"].([]any)
			for _, p := range ports {
				pm, ok := p.(map[string]any)
				if !ok {
					continue
				}
				if cp := toInt32(pm["containerPort"]); cp > 0 {
					return []corev1.ServicePort{{Name: "http", Port: cp, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt32(cp)}}
				}
			}
		}
	}
	return nil
}

func toInt32(v any) int32 {
	switch t := v.(type) {
	case int64:
		return int32(t)
	case int:
		return int32(t)
	case float64:
		return int32(t)
	}
	return 0
}

// Rollback re-applies a prior revision's manifest and records a new revision.
func (c *Client) Rollback(ctx context.Context, release string, revision int) (Release, error) {
	cur, ok, err := c.Get(ctx, release)
	if err != nil {
		return Release{}, err
	}
	if !ok {
		return Release{}, fmt.Errorf("release %q not found", release)
	}
	var target *Revision
	for i := range cur.History {
		if cur.History[i].Revision == revision {
			target = &cur.History[i]
			break
		}
	}
	if target == nil {
		return Release{}, fmt.Errorf("revision %d not found", revision)
	}
	if err := c.applyManifest(ctx, target.Manifest, release); err != nil {
		return Release{}, err
	}
	cur.Revision++
	cur.Status = "deployed"
	cur.UpdatedAt = time.Now().UnixMilli()
	cur.Ref = target.Ref
	cur.ChartPath = target.ChartPath
	cur.ChartVersion = target.ChartVersion
	cur.AppVersion = target.AppVersion
	cur.Objects = target.Objects
	cur.History = append(cur.History, Revision{
		Revision: cur.Revision, Ref: target.Ref, ChartPath: target.ChartPath, Values: target.Values,
		ChartVersion: target.ChartVersion, AppVersion: target.AppVersion,
		CreatedAt: cur.UpdatedAt, Objects: target.Objects, Manifest: target.Manifest,
	})
	if _, err := c.recordRevision(ctx, cur); err != nil {
		return Release{}, err
	}
	return cur, nil
}

// Uninstall deletes every object of the release (newest revision) and its
// release record.
func (c *Client) Uninstall(ctx context.Context, release string) (bool, error) {
	cur, ok, err := c.Get(ctx, release)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	if len(cur.History) > 0 {
		last := cur.History[len(cur.History)-1]
		// Delete in reverse order (dependents first is best-effort).
		for i := len(last.Objects) - 1; i >= 0; i-- {
			_ = c.deleteObject(ctx, last.Objects[i])
		}
	}
	if err := c.deleteReleaseState(ctx, release); err != nil {
		return false, err
	}
	return true, nil
}

// applyManifest parses + applies every doc (server-side create-or-update).
func (c *Client) applyManifest(ctx context.Context, manifest, release string) error {
	docs := strings.Split(manifest, "\n---")
	for _, doc := range docs {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var u unstructured.Unstructured
		if err := yaml.Unmarshal([]byte(doc), &u.Object); err != nil {
			return err
		}
		if len(u.Object) == 0 || u.GetKind() == "" {
			continue
		}
		labels := u.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[releaseLabel] = release
		labels["app.kubernetes.io/managed-by"] = "workspace-gateway"
		u.SetLabels(labels)
		u.SetNamespace(c.namespace)
		if err := c.applyObject(ctx, &u); err != nil {
			return fmt.Errorf("apply %s/%s: %w", u.GetKind(), u.GetName(), err)
		}
	}
	return nil
}

func (c *Client) applyObject(ctx context.Context, u *unstructured.Unstructured) error {
	mapping, err := c.mapper.RESTMapping(u.GroupVersionKind().GroupKind(), u.GroupVersionKind().Version)
	if err != nil {
		return err
	}
	ri := c.dyn.Resource(mapping.Resource).Namespace(c.namespace)
	cur, err := ri.Get(ctx, u.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err := ri.Create(ctx, u, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	u.SetResourceVersion(cur.GetResourceVersion())
	// Preserve an assigned ClusterIP for Services.
	if u.GetKind() == "Service" {
		if ip, found, _ := unstructured.NestedString(cur.Object, "spec", "clusterIP"); found && ip != "" {
			_ = unstructured.SetNestedField(u.Object, ip, "spec", "clusterIP")
		}
	}
	_, err = ri.Update(ctx, u, metav1.UpdateOptions{})
	return err
}

func (c *Client) deleteObject(ctx context.Context, o Object) error {
	gv, err := schema.ParseGroupVersion(o.APIVersion)
	if err != nil {
		return err
	}
	gvk := gv.WithKind(o.Kind)
	mapping, err := c.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return err
	}
	return c.dyn.Resource(mapping.Resource).Namespace(c.namespace).Delete(ctx, o.Name, metav1.DeleteOptions{})
}

func validReleaseName(s string) bool {
	if s == "" || len(s) > 53 {
		return false
	}
	for i, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
		if !ok {
			return false
		}
		if (i == 0 || i == len(s)-1) && r == '-' {
			return false
		}
	}
	return true
}

var _ = strconv.Itoa
var _ = runtime.NewScheme
