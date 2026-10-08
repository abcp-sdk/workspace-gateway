package servicesmgr

import "testing"

func TestMirrorDockerHubImage(t *testing.T) {
	const host = "artifact.worker.svc.cluster.local"
	cases := []struct{ in, want string }{
		// Docker Hub official image (bare) -> library/
		{"python:3.14-slim", host + "/library/python:3.14-slim"},
		{"redis", host + "/library/redis"},
		{"nginx:1.27-alpine", host + "/library/nginx:1.27-alpine"},
		// Docker Hub namespace
		{"grafana/grafana:11", host + "/grafana/grafana:11"},
		// explicit docker.io
		{"docker.io/library/python:3.13-slim", host + "/library/python:3.13-slim"},
		{"docker.io/nvidia/cuda:13.0.0-base-ubuntu24.04", host + "/nvidia/cuda:13.0.0-base-ubuntu24.04"},
		{"index.docker.io/library/alpine:3.20", host + "/library/alpine:3.20"},
		// another registry: UNCHANGED
		{"ghcr.io/owner/app:1", "ghcr.io/owner/app:1"},
		{"quay.io/org/app:2", "quay.io/org/app:2"},
		{"registry.k8s.io/pause:3.9", "registry.k8s.io/pause:3.9"},
		{"localhost:5000/app:1", "localhost:5000/app:1"},
		// already-artifact: UNCHANGED
		{host + "/coding-workspace/x:1", host + "/coding-workspace/x:1"},
		// empty host disables the rewrite
	}
	for _, c := range cases {
		if got := MirrorDockerHubImage(c.in, host); got != c.want {
			t.Errorf("MirrorDockerHubImage(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// empty registry host = no rewrite
	if got := MirrorDockerHubImage("python:3.14-slim", ""); got != "python:3.14-slim" {
		t.Errorf("empty host: got %q, want unchanged", got)
	}
}
