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
		"maven/_upstream/dl.google.com/",
		"maven/_upstream/plugins.gradle.org/",
		"maven/_upstream/repo.clojars.org/",
		"maven/_upstream/jitpack.io/",
		"maven/_upstream/repo.spring.io/",
		"allowInsecureProtocol = true",
		`"$H/.config/pip/pip.conf"`,
		`"$H/.cargo/config.toml"`,
		`"$H/.swiftpm/configuration/mirrors.json"`,
		`"$H/.nuget/NuGet/NuGet.Config"`,
		"allowInsecureConnections",
		"/mnt/f-gemrc/cfg",
		"/mnt/f-condarc/cfg",
		"/mnt/f-composer/cfg",
		"/mnt/f-npmrc/cfg",
		"/mnt/f-rprofile/cfg",
		"/mnt/f-cpan/cfg",
		"/mnt/f-luarocks/cfg",
		"/mnt/f-nixconf/cfg",
		`"$H/.conan2/remotes.json"`,
		`"$H/.opam/repo"`,
	} {
		if !strings.Contains(b.Script, want) {
			t.Errorf("script missing %q", want)
		}
	}
	// Cache/repo roots must be writable in the worker container.
	writable := map[string]bool{}
	files := map[string]bool{}
	for _, m := range b.Mounts {
		writable[m.Path] = m.Writable
		files[m.Path] = m.File
	}
	for _, p := range []string{"/root/.m2", "/root/.gradle", "/root/.cargo", "/root/.conan2", "/root/.opam"} {
		if !writable[p] {
			t.Errorf("%s must be Writable (cache/repo root)", p)
		}
	}
	if writable["/etc/apt/sources.list.d"] {
		t.Error("apt config dir should be read-only")
	}
	for _, p := range []string{"/root/.gemrc", "/root/.condarc", "/root/.composer/config.json", "/root/.npmrc", "/root/.Rprofile"} {
		if !files[p] {
			t.Errorf("%s must be a FILE mount (subPath)", p)
		}
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
