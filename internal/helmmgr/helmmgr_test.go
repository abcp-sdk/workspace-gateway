package helmmgr

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/fake"
	"testing"
)

// fakeMapper treats everything as namespaced (tests don't hit a cluster).
type fakeMapper struct{ meta.RESTMapper }

func (fakeMapper) RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	return &meta.RESTMapping{Scope: meta.RESTScopeNamespace}, nil
}

func writeChart(t *testing.T, tmpl string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte("apiVersion: v2\nname: demo\nversion: 0.1.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tplDir := filepath.Join(dir, "templates")
	if err := os.MkdirAll(tplDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "all.yaml"), []byte(tmpl), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestTemplateAndValidate(t *testing.T) {
	c := &Client{mapper: fakeMapper{}, namespace: "worker"}
	dir := writeChart(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: demo-cm
data:
  k: {{ .Values.v | quote }}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: demo
spec:
  template:
    spec:
      containers:
        - name: app
          image: {{ .Values.image }}
`)
	manifest, objs, err := c.Template(dir, RenderOptions{Release: "demo", ChartPath: ".", ValuesYAML: "v: hi\nimage: nginx:1"})
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	if len(objs) != 2 {
		t.Fatalf("objects = %+v", objs)
	}
	if !contains(manifest, "hi") || !contains(manifest, "nginx:1") {
		t.Fatalf("values not rendered:\n%s", manifest)
	}
}

func TestValidateRejectsDangerous(t *testing.T) {
	c := &Client{mapper: fakeMapper{}, namespace: "worker"}
	cases := map[string]string{
		"cluster-scoped kind": `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: x
`,
		"hostPath": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: d
spec:
  template:
    spec:
      volumes:
        - name: host
          hostPath:
            path: /etc
      containers:
        - name: a
          image: x
`,
		"hostNetwork": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: d
spec:
  template:
    spec:
      hostNetwork: true
      containers:
        - name: a
          image: x
`,
		"privileged": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: d
spec:
  template:
    spec:
      containers:
        - name: a
          image: x
          securityContext:
            privileged: true
`,
	}
	for name, doc := range cases {
		if _, err := c.validate(doc); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

func TestValidateRejectsForeignNamespace(t *testing.T) {
	c := &Client{mapper: fakeMapper{}, namespace: "worker"}
	doc := `apiVersion: v1
kind: ConfigMap
metadata:
  name: x
  namespace: kube-system
`
	if _, err := c.validate(doc); err == nil {
		t.Fatal("expected foreign-namespace rejection")
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestInjectSlotLabel(t *testing.T) {
	in := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: demo
spec:
  template:
    metadata:
      labels:
        app: demo
    spec:
      containers:
        - name: app
          image: nginx
---
apiVersion: v1
kind: Service
metadata:
  name: demo
spec:
  selector:
    app: demo
  ports:
    - port: 80
      targetPort: 8080
`
	out, err := injectSlotLabel(in, "web-green", "green")
	if err != nil {
		t.Fatal(err)
	}
	var dep map[string]any
	for _, doc := range splitDocs(out) {
		var u map[string]any
		if err := yaml.Unmarshal([]byte(doc), &u); err != nil {
			t.Fatal(err)
		}
		if u["kind"] == "Deployment" {
			dep = u
		}
	}
	if dep == nil {
		t.Fatal("no deployment")
	}
	labels := dep["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)["labels"].(map[string]any)
	if labels["workspace/helm-slot"] != "green" || labels["workspace/helm-release"] != "web-green" {
		t.Fatalf("labels = %+v", labels)
	}
}

func TestRouterPortsFromManifest(t *testing.T) {
	m := `apiVersion: v1
kind: Service
metadata:
  name: demo
spec:
  ports:
    - port: 80
      targetPort: 8080
`
	ports := routerPortsFromManifest(m)
	if len(ports) != 1 || ports[0].Port != 80 || ports[0].TargetPort.IntVal != 8080 {
		t.Fatalf("ports = %+v", ports)
	}
}

func TestRouterPortsFallback(t *testing.T) {
	m := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: demo
spec:
  template:
    spec:
      containers:
        - name: app
          image: nginx
          ports:
            - containerPort: 3000
`
	ports := routerPortsFromManifest(m)
	if len(ports) != 1 || ports[0].Port != 3000 {
		t.Fatalf("ports = %+v", ports)
	}
}

func splitDocs(m string) []string {
	var out []string
	for _, d := range strings.Split(m, "\n---") {
		if strings.TrimSpace(d) != "" {
			out = append(out, d)
		}
	}
	return out
}

// newStateClient builds a Client whose k8s clients are in-memory fakes, so the
// release-state (ConfigMap) split + pruning can be exercised without a cluster.
func newStateClient(historyMax int) *Client {
	return &Client{
		cs:         fake.NewSimpleClientset(),
		mapper:     fakeMapper{},
		namespace:  "worker",
		historyMax: historyMax,
	}
}

// recordFakeRevision appends revision n to the release and persists it through
// the real write path (revision body + head record + prune).
func recordFakeRevision(t *testing.T, c *Client, release string, n int) {
	t.Helper()
	cur, _, err := c.Get(context.Background(), release)
	if err != nil {
		t.Fatal(err)
	}
	cur.Name = release
	cur.Revision = n
	cur.Status = "deployed"
	cur.UpdatedAt = int64(n)
	cur.History = append(cur.History, Revision{
		Revision: n, Ref: "main", ChartPath: ".",
		CreatedAt: int64(n), Objects: []Object{{Kind: "ConfigMap", Name: fmt.Sprintf("cm-%d", n)}},
		Manifest: fmt.Sprintf("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm-%d\n", n),
	})
	if _, err := c.recordRevision(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
}

// TestStateOneObjectPerRevision pins the fix for the 1 MiB failure: the head
// ConfigMap holds ONLY metadata (no manifest), each revision lives in its own
// ConfigMap, and the history survives the split.
func TestStateOneObjectPerRevision(t *testing.T) {
	c := newStateClient(0) // 0 = package default (bounded)
	recordFakeRevision(t, c, "web", 1)
	recordFakeRevision(t, c, "web", 2)

	head, err := c.cs.CoreV1().ConfigMaps("worker").Get(context.Background(), c.cmName("web"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(head.Data["release"], "apiVersion") {
		t.Fatalf("head must not carry a manifest:\n%s", head.Data["release"])
	}
	if head.Labels[recordKindLabel] != recordHead {
		t.Fatalf("head labels = %+v", head.Labels)
	}

	rel, ok, err := c.Get(context.Background(), "web")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if len(rel.History) != 2 || rel.History[0].Revision != 1 || rel.History[1].Revision != 2 {
		t.Fatalf("history = %+v", rel.History)
	}
	if rel.History[1].Manifest == "" || len(rel.History[1].Objects) != 1 {
		t.Fatalf("revision body not materialized: %+v", rel.History[1])
	}

	bodies, err := c.cs.CoreV1().ConfigMaps("worker").List(context.Background(), metav1.ListOptions{
		LabelSelector: releaseLabel + "=web," + recordKindLabel + "=" + recordRevision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bodies.Items) != 2 {
		t.Fatalf("revision bodies = %d, want 2", len(bodies.Items))
	}
}

// TestStatePrunesHistoryMax proves old revisions are pruned so the stored state
// stays bounded (and the head never grows with the revision count).
func TestStatePrunesHistoryMax(t *testing.T) {
	c := newStateClient(3)
	for n := 1; n <= 10; n++ {
		recordFakeRevision(t, c, "web", n)
	}
	rel, _, err := c.Get(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	if len(rel.History) != 3 {
		t.Fatalf("history len = %d, want 3: %+v", len(rel.History), rel.History)
	}
	if rel.History[0].Revision != 8 || rel.History[2].Revision != 10 {
		t.Fatalf("kept revisions = %+v, want 8..10", rel.History)
	}
	bodies, _ := c.cs.CoreV1().ConfigMaps("worker").List(context.Background(), metav1.ListOptions{
		LabelSelector: releaseLabel + "=web," + recordKindLabel + "=" + recordRevision,
	})
	if len(bodies.Items) != 3 {
		t.Fatalf("revision bodies = %d, want 3", len(bodies.Items))
	}
}

// TestStateUnlimitedHistory covers HistoryMax < 0 (keep everything).
func TestStateUnlimitedHistory(t *testing.T) {
	c := newStateClient(-1)
	for n := 1; n <= 30; n++ {
		recordFakeRevision(t, c, "web", n)
	}
	rel, _, err := c.Get(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	if len(rel.History) != 30 {
		t.Fatalf("history len = %d, want 30", len(rel.History))
	}
}

// TestStateMigratesLegacyHead covers a release written BEFORE the split (all
// revisions inside the head blob): Get must materialize them AND migrate the
// state to one-object-per-revision.
func TestStateMigratesLegacyHead(t *testing.T) {
	c := newStateClient(0)
	legacy := Release{
		Name: "web", Namespace: "worker", Creator: "t", Revision: 2, Status: "deployed", UpdatedAt: 2,
		History: []Revision{
			{Revision: 1, Manifest: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm-1\n", Objects: []Object{{Kind: "ConfigMap", Name: "cm-1"}}},
			{Revision: 2, Manifest: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm-2\n", Objects: []Object{{Kind: "ConfigMap", Name: "cm-2"}}},
		},
	}
	b, _ := json.Marshal(legacy)
	if _, err := c.cs.CoreV1().ConfigMaps("worker").Create(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: c.cmName("web"), Namespace: "worker",
			Labels: map[string]string{releaseLabel: "web"},
		},
		Data: map[string]string{"release": string(b)},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	rel, ok, err := c.Get(context.Background(), "web")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if len(rel.History) != 2 || rel.History[1].Manifest == "" {
		t.Fatalf("legacy history not materialized: %+v", rel.History)
	}
	head, err := c.cs.CoreV1().ConfigMaps("worker").Get(context.Background(), c.cmName("web"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(head.Data["release"], "apiVersion") {
		t.Fatalf("head not migrated (still carries manifests):\n%s", head.Data["release"])
	}
	bodies, _ := c.cs.CoreV1().ConfigMaps("worker").List(context.Background(), metav1.ListOptions{
		LabelSelector: releaseLabel + "=web," + recordKindLabel + "=" + recordRevision,
	})
	if len(bodies.Items) != 2 {
		t.Fatalf("migrated revision bodies = %d, want 2", len(bodies.Items))
	}
}

// TestListSkipsRevisionBodies: List must surface one row per release, never one
// per stored revision.
func TestListSkipsRevisionBodies(t *testing.T) {
	c := newStateClient(0)
	for n := 1; n <= 4; n++ {
		recordFakeRevision(t, c, "web", n)
	}
	rels, err := c.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) != 1 || rels[0].Name != "web" {
		t.Fatalf("list = %+v", rels)
	}
	if len(rels[0].History) != 0 {
		t.Fatalf("List must not materialize history: %+v", rels[0].History)
	}
}
