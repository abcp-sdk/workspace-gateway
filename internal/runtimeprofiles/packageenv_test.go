package runtimeprofiles

import "testing"

func TestPackageEnv(t *testing.T) {
	if PackageEnv("") != nil {
		t.Fatal("empty base must yield nil")
	}
	env := PackageEnv("http://artifact.worker.svc.cluster.local/")
	want := map[string]string{
		"PIP_INDEX_URL":       "http://artifact.worker.svc.cluster.local/artifacts/pypi/simple/",
		"PIP_TRUSTED_HOST":    "artifact.worker.svc.cluster.local",
		"NPM_CONFIG_REGISTRY": "http://artifact.worker.svc.cluster.local/artifacts/npm/",
		"GOPROXY":             "http://artifact.worker.svc.cluster.local/artifacts/go",
		"GOSUMDB":             "off",
		"PUB_HOSTED_URL":      "http://artifact.worker.svc.cluster.local/artifacts/pub",
		"HEX_MIRROR":          "http://artifact.worker.svc.cluster.local/artifacts/hex/",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s: got %q want %q", k, env[k], v)
		}
	}
}

func TestRenderNoPackageUpstream(t *testing.T) {
	r := DefaultSettings().Render(false, 0)
	if r.Env != nil {
		t.Fatalf("no PackageUpstream must yield no env, got %v", r.Env)
	}
}

func TestRenderNeverInjectsPackageEnv(t *testing.T) {
	// Render is shared with user services; package env must NOT leak there.
	s := DefaultSettings()
	s.PackageUpstream = "http://artifact.worker.svc.cluster.local"
	r := s.Render(false, 0)
	if r.Env != nil {
		t.Fatalf("Render must not inject package env, got %v", r.Env)
	}
}
