// Package helmmgr renders a Helm chart from a repository (using the Helm SDK
// templating packages only) and applies the result to the managed namespace via
// a dynamic client, with a strict safety filter. Release state (revision
// history + rendered manifests) is kept in a ConfigMap per release.
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
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
)

// Allowed kinds a rendered manifest may contain. Anything else is refused, as
// are cluster-scoped resources, RBAC, CRDs and privileged pod features.
var allowedKinds = map[string]bool{
	"ConfigMap":             true,
	"Secret":                true,
	"Service":               true,
	"ServiceAccount":        true,
	"PersistentVolumeClaim": true,
	"Deployment":            true,
	"StatefulSet":           true,
	"DaemonSet":             true,
	"Job":                   true,
	"CronJob":               true,
	"Ingress":               true,
	"NetworkPolicy":         true,
	"HorizontalPodAutoscaler": true,
	"PodDisruptionBudget":   true,
}

// Client renders + applies Helm charts in one namespace.
type Client struct {
	dyn       dynamic.Interface
	cs        kubernetes.Interface
	mapper    meta.RESTMapper
	namespace string
}

// Config configures a Client.
type Config struct{ Namespace string }

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
	return &Client{dyn: dyn, cs: cs, mapper: restmapper.NewDiscoveryRESTMapper(gr), namespace: ns}, nil
}

// Object is one rendered + applied resource.
type Object struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
}

// Revision is one stored release revision.
type Revision struct {
	Revision  int       `json:"revision"`
	Ref       string    `json:"ref"`
	ChartPath string    `json:"chartPath"`
	Values    string    `json:"values"`
	CreatedAt int64     `json:"createdAt"`
	Objects   []Object  `json:"objects"`
	Manifest  string    `json:"manifest"`
}

// Release is a Helm release managed by the gateway.
type Release struct {
	Name      string     `json:"name"`
	Namespace string     `json:"namespace"`
	Creator   string     `json:"creator"`
	Session   string     `json:"session"`
	Ref       string     `json:"ref"`
	ChartPath string     `json:"chartPath"`
	Revision  int        `json:"revision"`
	Status    string     `json:"status"`
	UpdatedAt int64      `json:"updatedAt"`
	History   []Revision `json:"history"`
}

const (
	releaseLabel   = "workspace/helm-release"
	releaseCMName  = "helm-release-"
	annoReleaseName = "workspace/helm-release-name"
)

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
}

// Template renders the chart at chartDir (an extracted repo root + ChartPath)
// into a filtered, namespace-pinned manifest. It returns the rendered docs and
// the applied objects (dry-run = no apply).
func (c *Client) Template(chartDir string, o RenderOptions) (string, []Object, error) {
	ch, err := loader.LoadDir(chartDir)
	if err != nil {
		return "", nil, fmt.Errorf("load chart: %w", err)
	}
	vals := map[string]any{}
	if strings.TrimSpace(o.ValuesYAML) != "" {
		if err := yaml.Unmarshal([]byte(o.ValuesYAML), &vals); err != nil {
			return "", nil, fmt.Errorf("parse values: %w", err)
		}
	}
	relOpts := chartutil.ReleaseOptions{Name: o.Release, Namespace: c.namespace, Revision: 1, IsInstall: true}
	caps := chartutil.DefaultCapabilities
	renderVals, err := chartutil.ToRenderValues(ch, vals, relOpts, caps)
	if err != nil {
		return "", nil, fmt.Errorf("render values: %w", err)
	}
	rendered, err := engine.Render(ch, renderVals)
	if err != nil {
		return "", nil, fmt.Errorf("render: %w", err)
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
		return "", nil, err
	}
	return manifest, objects, nil
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

// ---- release state (ConfigMap-backed) ----

func (c *Client) cmName(release string) string { return releaseCMName + release }

// Get returns a release, or ok=false when absent.
func (c *Client) Get(ctx context.Context, release string) (Release, bool, error) {
	cm, err := c.cs.CoreV1().ConfigMaps(c.namespace).Get(ctx, c.cmName(release), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Release{}, false, nil
	}
	if err != nil {
		return Release{}, false, err
	}
	var r Release
	if err := json.Unmarshal([]byte(cm.Data["release"]), &r); err != nil {
		return Release{}, false, err
	}
	return r, true, nil
}

func (c *Client) put(ctx context.Context, r Release) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: c.cmName(r.Name), Namespace: c.namespace,
			Labels: map[string]string{releaseLabel: r.Name, "app.kubernetes.io/managed-by": "workspace-gateway"},
		},
		Data: map[string]string{"release": string(b)},
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

// List returns every release in the namespace (newest first).
func (c *Client) List(ctx context.Context) ([]Release, error) {
	cms, err := c.cs.CoreV1().ConfigMaps(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: releaseLabel})
	if err != nil {
		return nil, err
	}
	out := make([]Release, 0, len(cms.Items))
	for i := range cms.Items {
		var r Release
		if err := json.Unmarshal([]byte(cms.Items[i].Data["release"]), &r); err != nil {
			continue
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	return out, nil
}

// ---- apply ----

// Apply renders + applies a chart as a NEW or updated release. It records a
// new revision in the release history.
func (c *Client) Apply(ctx context.Context, chartDir string, o RenderOptions, creator, session string) (Release, error) {
	if !validReleaseName(o.Release) {
		return Release{}, fmt.Errorf("release name must be a DNS-1123 label")
	}
	manifest, objects, err := c.Template(chartDir, o)
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
	cur.Revision++
	cur.Status = "deployed"
	cur.UpdatedAt = time.Now().UnixMilli()
	cur.History = append(cur.History, Revision{
		Revision: cur.Revision, Ref: o.Ref, ChartPath: o.ChartPath, Values: o.ValuesYAML,
		CreatedAt: cur.UpdatedAt, Objects: objects, Manifest: manifest,
	})
	if err := c.put(ctx, cur); err != nil {
		return Release{}, err
	}
	return cur, nil
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
	cur.History = append(cur.History, Revision{
		Revision: cur.Revision, Ref: target.Ref, ChartPath: target.ChartPath, Values: target.Values,
		CreatedAt: cur.UpdatedAt, Objects: target.Objects, Manifest: target.Manifest,
	})
	if err := c.put(ctx, cur); err != nil {
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
	if err := c.cs.CoreV1().ConfigMaps(c.namespace).Delete(ctx, c.cmName(release), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
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
