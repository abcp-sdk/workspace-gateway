// Package sandboxbootstrap renders the package-source bootstrap applied to
// every sandbox: an init container writes the FILE-configured package
// managers' config (apt/apk/maven/gradle/pip/cargo/SPM/nuget/git) into shared
// emptyDirs that are mounted over the worker container's matching config dirs.
//
// Everything is derived from the registry URL at POD-CREATE time (never baked
// into an image), so changing the registry needs only an env change on the
// gateway — no image rebuild. The env-configurable managers (pip/npm/go/cargo/
// pub/hex) are handled separately by runtimeprofiles.Render (sandbox env).
package sandboxbootstrap

import (
	"fmt"
	"strings"

	"github.com/abcp-sdk/workspace-gateway/internal/sandboxmgr"
)

// configMounts are PURE config dirs: the bootstrap writes them, the worker only
// reads them, so a read-only overlay is correct. Overlaying a directory also
// HIDES any file the image had there (e.g. Debian's own
// /etc/apt/sources.list.d/debian.sources) — exactly what we want: only the
// artifact source remains.
//
// cacheMounts are tool HOME dirs that are BOTH config AND cache/repo roots
// (Maven local repo, GRADLE_USER_HOME, CARGO_HOME). They MUST stay writable or
// `mvn`/`gradle`/`cargo` fail with EROFS on first dependency resolution, so
// they are mounted read-WRITE.

// Build returns the bootstrap for `base` (the artifact registry root, e.g.
// "http://artifact.worker.svc.cluster.local") and `home` (the worker's HOME,
// default "/root"). Returns nil when base is empty (no package source
// configured → historical behaviour) or image is empty.
func Build(base, image, home string) *sandboxmgr.Bootstrap {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" || image == "" {
		return nil
	}
	if home == "" {
		home = "/root"
	}
	return &sandboxmgr.Bootstrap{Image: image, Script: script(base, home), Mounts: mounts(home)}
}

// mounts lists the config/cache dirs for a worker whose HOME is `home`.
func mounts(home string) []sandboxmgr.BootstrapMount {
	return []sandboxmgr.BootstrapMount{
		{Name: "cfg-apt", Path: "/etc/apt/sources.list.d"},
		{Name: "cfg-apk", Path: "/etc/apk"},
		// Writable: config + local repo/cache.
		{Name: "cfg-m2", Path: home + "/.m2", Writable: true},
		{Name: "cfg-gradle", Path: home + "/.gradle", Writable: true},
		{Name: "cfg-cargo", Path: home + "/.cargo", Writable: true},
		{Name: "cfg-pip", Path: home + "/.config/pip"},
		{Name: "cfg-swiftpm", Path: home + "/.swiftpm/configuration"},
		{Name: "cfg-nuget", Path: home + "/.nuget/NuGet"},
	}
}

// script renders the POSIX-sh bootstrap. `$A` is the registry root; the tools'
// plain-HTTP quirks are baked in (Gradle allowInsecureProtocol, NuGet
// allowInsecureConnections, apt trusted=yes, …). Content mirrors the easyops
// bootstrap so easyvcs and abc-protocol sandboxes resolve identically.
// NOTE: the apt suite (`trixie`) and apk version (`v3.24`) are chosen to match
// the deployment's Debian-trixie / Alpine-3.24 toolchain images; a different
// distro needs different suites.
func script(base, home string) string {
	return fmt.Sprintf(`set -eu
A=%q
H=%q
# --- apt (Debian) — only the artifact source (the image's own sources are
# hidden because /etc/apt/sources.list.d is overlaid by an emptyDir).
if [ -d /etc/apt/sources.list.d ]; then
  printf 'deb [trusted=yes] %%s/artifacts/debian/debian trixie main\n' "$A" > /etc/apt/sources.list.d/artifact.list
fi
# --- apk (Alpine) — /etc/apk is overlaid, so this is the only repository.
if [ -d /etc/apk ]; then
  printf '%%s/artifacts/apk/v3.24/main\n' "$A" > /etc/apk/repositories
fi
# --- maven
mkdir -p "$H/.m2"
printf '<settings><mirrors><mirror><id>artifact</id><mirrorOf>*</mirrorOf><url>%%s/artifacts/maven/</url></mirror></mirrors></settings>' "$A" > "$H/.m2/settings.xml"
# --- gradle — ORDER MATTERS: google then gradle then central (shared cache, no JAR fallback).
mkdir -p "$H/.gradle"
printf 'allprojects{repositories{clear();maven{url "%%s/artifacts/maven.google/"; allowInsecureProtocol = true};maven{url "%%s/artifacts/maven.gradle/"; allowInsecureProtocol = true};maven{url "%%s/artifacts/maven/"; allowInsecureProtocol = true}}}\nsettingsEvaluated{s->s.pluginManagement{repositories{clear();maven{url "%%s/artifacts/maven.google/"; allowInsecureProtocol = true};maven{url "%%s/artifacts/maven.gradle/"; allowInsecureProtocol = true};maven{url "%%s/artifacts/maven/"; allowInsecureProtocol = true}}}}' "$A" "$A" "$A" "$A" "$A" "$A" > "$H/.gradle/init.gradle"
# --- pip
mkdir -p "$H/.config/pip"
printf '[global]\nindex-url = %%s/artifacts/pypi/simple/\ntrusted-host = %%s\n' "$A" "${A#*://}" > "$H/.config/pip/pip.conf"
# --- cargo
mkdir -p "$H/.cargo"
printf '[source.crates-io]\nreplace-with="artifact"\n[source.artifact]\nregistry="sparse+%%s/artifacts/cargo/index/"\n' "$A" > "$H/.cargo/config.toml"
# --- SwiftPM: libgit2 ignores git insteadOf, so seed per-repo mirrors.
# This is a SEED list (the common apple/* libs), not exhaustive — add more as needed.
mkdir -p "$H/.swiftpm/configuration"
printf '{"version":1,"object":[{"original":"https://github.com/apple/swift-argument-parser.git","mirror":"%%s/artifacts/git/github.com/apple/swift-argument-parser.git"},{"original":"https://github.com/apple/swift-log.git","mirror":"%%s/artifacts/git/github.com/apple/swift-log.git"},{"original":"https://github.com/apple/swift-nio.git","mirror":"%%s/artifacts/git/github.com/apple/swift-nio.git"},{"original":"https://github.com/apple/swift-collections.git","mirror":"%%s/artifacts/git/github.com/apple/swift-collections.git"},{"original":"https://github.com/apple/swift-crypto.git","mirror":"%%s/artifacts/git/github.com/apple/swift-crypto.git"},{"original":"https://github.com/apple/swift-syntax.git","mirror":"%%s/artifacts/git/github.com/apple/swift-syntax.git"},{"original":"https://github.com/apple/swift-atomics.git","mirror":"%%s/artifacts/git/github.com/apple/swift-atomics.git"},{"original":"https://github.com/apple/swift-system.git","mirror":"%%s/artifacts/git/github.com/apple/swift-system.git"}]}' "$A" "$A" "$A" "$A" "$A" "$A" "$A" "$A" > "$H/.swiftpm/configuration/mirrors.json"
# --- nuget
mkdir -p "$H/.nuget/NuGet"
printf '<?xml version="1.0"?><configuration><packageSources><clear/><add key="artifact" value="%%s/artifacts/nuget/v3/index.json" allowInsecureConnections="true"/></packageSources></configuration>' "$A" > "$H/.nuget/NuGet/NuGet.Config"
# --- git (CLI) — rewrite github to the artifact git mirror.
git config --global url."$A/artifacts/git/github.com/".insteadOf https://github.com/ 2>/dev/null || true
`, base, home)
}
