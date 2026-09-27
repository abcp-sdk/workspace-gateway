package helmmgr

import (
	"strings"

	"k8s.io/apimachinery/pkg/util/yaml"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
