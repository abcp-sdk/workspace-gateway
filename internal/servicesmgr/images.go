package servicesmgr

import "strings"

// MirrorDockerHubImage rewrites a Docker Hub image reference to `registryHost`
// (the deployment registry — artifact — which pull-through-mirrors docker.io),
// so a service that names a public image (`python:3.14-slim`,
// `docker.io/library/redis:7`, `nvidia/cuda:13`) pulls from artifact instead of
// hitting docker.io directly (which is unreachable from the cluster).
//
// Only Docker Hub is rewritten. Any other explicit registry (ghcr.io, quay.io,
// registry.k8s.io, or an already-artifact ref) is returned UNCHANGED — those are
// not mirrored by artifact, so blindly rewriting them would break the pull.
//
// registryHost == "" disables the rewrite (the image is returned as-is).
func MirrorDockerHubImage(image, registryHost string) string {
	image = strings.TrimSpace(image)
	registryHost = strings.TrimRight(strings.TrimSpace(registryHost), "/")
	if image == "" || registryHost == "" {
		return image
	}

	repo := image
	if i := strings.IndexByte(image, '/'); i >= 0 {
		first := image[:i]
		// A first segment is a HOST iff it looks like one (contains '.' or ':',
		// or is "localhost"). Otherwise it is a Docker Hub namespace and the
		// reference has no explicit host.
		if strings.ContainsAny(first, ".:") || first == "localhost" {
			switch first {
			case "docker.io", "index.docker.io", "registry-1.docker.io":
				repo = image[i+1:]
			default:
				return image // another registry — leave it alone
			}
		}
	}

	// Docker Hub official images live under the implicit `library/` namespace;
	// their canonical path is `library/<name>`. Namespaced refs keep their path.
	if !strings.Contains(repo, "/") {
		repo = "library/" + repo
	}
	return registryHost + "/" + repo
}
