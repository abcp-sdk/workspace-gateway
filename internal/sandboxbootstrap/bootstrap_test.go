package sandboxbootstrap

import (
	"strings"
	"testing"
)

func TestBuildEmpty(t *testing.T) {
	if b := Build("", "alpine", "/root"); b != nil {
		t.Fatalf("empty base must yield nil, got %+v", b)
	}
	if b := Build("http://artifact.worker.svc.cluster.local", "", "/root"); b != nil {
		t.Fatalf("empty image must yield nil, got %+v", b)
	}
}

func TestBuildScriptAndMounts(t *testing.T) {
	b := Build("http://artifact.worker.svc.cluster.local/", "alpine:3.24", "/root")
	if b == nil {
		t.Fatal("expected a bootstrap")
	}
	if b.Image != "alpine:3.24" {
		t.Fatalf("image: %q", b.Image)
	}
	if len(b.Mounts) == 0 {
		t.Fatal("expected mounts")
	}
	// The base URL is trimmed of the trailing slash and baked into the script.
	if !strings.Contains(b.Script, `A="http://artifact.worker.svc.cluster.local"`) {
		t.Fatalf("script missing trimmed base: %s", b.Script)
	}
	// Every file-configured manager must appear.
	for _, want := range []string{
		"/etc/apt/sources.list.d/artifact.list",
		"/etc/apk/repositories",
		`"$H/.m2/settings.xml"`,
		`"$H/.gradle/init.gradle"`,
		"maven.google",
		"maven.gradle",
		"allowInsecureProtocol = true",
		`"$H/.config/pip/pip.conf"`,
		`"$H/.cargo/config.toml"`,
		`"$H/.swiftpm/configuration/mirrors.json"`,
		`"$H/.nuget/NuGet/NuGet.Config"`,
		"allowInsecureConnections",
	} {
		if !strings.Contains(b.Script, want) {
			t.Errorf("script missing %q", want)
		}
	}
	// Cache/repo roots must be writable in the worker container.
	writable := map[string]bool{}
	for _, m := range b.Mounts {
		writable[m.Path] = m.Writable
	}
	for _, p := range []string{"/root/.m2", "/root/.gradle", "/root/.cargo"} {
		if !writable[p] {
			t.Errorf("%s must be Writable (cache/repo root)", p)
		}
	}
	if writable["/etc/apt/sources.list.d"] {
		t.Error("apt config dir should be read-only")
	}
}

func TestBuildHonorsHome(t *testing.T) {
	b := Build("http://artifact", "alpine:3.24", "/home/dev")
	if b == nil {
		t.Fatal("expected bootstrap")
	}
	if !strings.Contains(b.Script, `H="/home/dev"`) {
		t.Fatalf("script missing HOME: %s", b.Script)
	}
	found := false
	for _, m := range b.Mounts {
		if m.Path == "/home/dev/.m2" {
			found = true
		}
	}
	if !found {
		t.Fatal("mounts not HOME-relative")
	}
}
