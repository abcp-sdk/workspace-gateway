// Package sandboxbootstrap renders the package-source bootstrap applied to
// every sandbox: an init container writes each package manager's config into
// shared emptyDirs that are mounted over the worker container's matching
// config dirs (or, for single files, over the file via subPath).
//
// Everything is derived from the registry URL at POD-CREATE time (never baked
// into an image), so changing the registry needs only an env change on the
// gateway — no image rebuild. Env-configurable managers (pip/npm/go/cargo/pub/
// hex) are ALSO set via sandbox env (runtimeprofiles.PackageEnv); the config
// files here cover the rest and the file-based knobs.
package sandboxbootstrap

import (
	"fmt"
	"strings"

	"github.com/abcp-sdk/workspace-gateway/internal/sandboxmgr"
)

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

// mounts lists the config dirs/files for a worker whose HOME is `home`.
// Directory mounts overlay the whole dir (also hiding the image's own config);
// file mounts use subPath so only that file is replaced. `Writable` marks
// dirs that are also cache/repo roots (Maven/Gradle/Cargo/... must stay RW).
func mounts(home string) []sandboxmgr.BootstrapMount {
	return []sandboxmgr.BootstrapMount{
		// apt / apk (system dirs; overlay also hides the image's own sources).
		{Name: "cfg-apt", Path: "/etc/apt/sources.list.d"},
		{Name: "cfg-apk", Path: "/etc/apk"},
		// JVM: config + local repo/cache → writable.
		{Name: "cfg-m2", Path: home + "/.m2", Writable: true},
		{Name: "cfg-gradle", Path: home + "/.gradle", Writable: true},
		// pip (config dir only).
		{Name: "cfg-pip", Path: home + "/.config/pip"},
		// Rust: config + registry cache → writable.
		{Name: "cfg-cargo", Path: home + "/.cargo", Writable: true},
		// SwiftPM (config dir only).
		{Name: "cfg-swiftpm", Path: home + "/.swiftpm/configuration"},
		// NuGet (config dir only).
		{Name: "cfg-nuget", Path: home + "/.nuget/NuGet"},
		// Conan: config + cache → writable.
		{Name: "cfg-conan", Path: home + "/.conan2", Writable: true},
		// Cargo's own git deps use the git CLI's insteadOf; opam/dune below.
		{Name: "cfg-opam", Path: home + "/.opam", Writable: true},
		// Single-file configs (subPath mounts).
		{Name: "f-gemrc", Path: home + "/.gemrc", File: true},
		{Name: "f-condarc", Path: home + "/.condarc", File: true},
		{Name: "f-composer", Path: home + "/.composer/config.json", File: true},
		{Name: "f-npmrc", Path: home + "/.npmrc", File: true},
		{Name: "f-rprofile", Path: home + "/.Rprofile", File: true},
		{Name: "f-cpan", Path: home + "/.cpan/CPAN/MyConfig.pm", File: true},
		{Name: "f-luarocks", Path: home + "/.luarocks/config-5.4.lua", File: true},
		{Name: "f-nixconf", Path: home + "/.config/nix/nix.conf", File: true},
	}
}

// script renders the POSIX-sh bootstrap. `$A` is the registry root, `$H` the
// worker HOME. Plain-HTTP quirks are baked in (Gradle allowInsecureProtocol,
// NuGet allowInsecureConnections, apt trusted=yes, …).
//
// NOTE: apt suite (`trixie`) and apk version (`v3.24`) match the deployment's
// Debian-trixie / Alpine-3.24 toolchain images; a different distro needs
// different suites. SwiftPM mirrors are a SEED list (common apple/* libs),
// not exhaustive — add more as needed.
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
# --- gradle — ORDER MATTERS: third-party hosts first, then Central LAST
# (artifact maven targets share one cache; Gradle fixes a JAR to the repo that
# served its POM with no fallback). Covers google + gradle plugin portal +
# clojars + jitpack + spring, both in allprojects and pluginManagement.
# The named targets (maven.google, …) were REMOVED in artifact #40 — the
# supported form is the generic maven/_upstream/<host>/ route (host-only;
# artifact appends the base path). Central stays maven/ and goes LAST.
mkdir -p "$H/.gradle"
REPOS='maven{url "'"$A"'/artifacts/maven/_upstream/dl.google.com/"; allowInsecureProtocol = true};maven{url "'"$A"'/artifacts/maven/_upstream/plugins.gradle.org/"; allowInsecureProtocol = true};maven{url "'"$A"'/artifacts/maven/_upstream/repo.clojars.org/"; allowInsecureProtocol = true};maven{url "'"$A"'/artifacts/maven/_upstream/jitpack.io/"; allowInsecureProtocol = true};maven{url "'"$A"'/artifacts/maven/_upstream/repo.spring.io/"; allowInsecureProtocol = true};maven{url "'"$A"'/artifacts/maven/"; allowInsecureProtocol = true}'
printf 'allprojects{repositories{clear();%%s}}\nsettingsEvaluated{s->s.pluginManagement{repositories{clear();%%s}}}' "$REPOS" "$REPOS" > "$H/.gradle/init.gradle"
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
# --- rubygems
printf -- '---\n:sources:\n- %%s/artifacts/rubygems/\n' "$A" > /mnt/f-gemrc/cfg
# --- conda
printf 'channels:\n  - %%s/artifacts/conda/pkgs/main\ndefault_channels:\n  - %%s/artifacts/conda/pkgs/main\n' "$A" "$A" > /mnt/f-condarc/cfg
# --- composer (PHP)
printf '{"config":{"secure-http":false},"repositories":{"packagist":{"type":"composer","url":"%%s/artifacts/composer/"}}}' "$A" > /mnt/f-composer/cfg
# --- npm (also for tools that read ~/.npmrc directly)
printf 'registry=%%s/artifacts/npm/\nstrict-ssl=false\n' "$A" > /mnt/f-npmrc/cfg
# --- cran (R): point the default repo at artifact
printf '.libPaths(Sys.getenv("R_LIBS_USER"))\noptions(repos = c(CRAN = "%%s/artifacts/cran/"))\n' "$A" > /mnt/f-rprofile/cfg
# --- cpan (Perl)
printf '%%%%CPAN::Config = (\n  "urllist" => [ "%%s/artifacts/cpan/" ],\n  "connect_to_internet_ok" => 1,\n);\n1;\n' "$A" > /mnt/f-cpan/cfg
# --- luarocks (Lua)
printf 'rocks_servers = { "%%s/artifacts/luarocks/" }\n' "$A" > /mnt/f-luarocks/cfg
# --- nix: substituter = the artifact nix cache
printf 'substituters = %%s/artifacts/nix/\ntrusted-public-keys = cache.nixos.org-1:6NCHdD59X431o0gWypbMrAURkbJ16ZPMQFGspcDShjY=\n' "$A" > /mnt/f-nixconf/cfg
# --- conan (C++): add artifact as a remote
mkdir -p "$H/.conan2"
printf '[registries]\nartifact = %%s/artifacts/conan/\n' "$A" > "$H/.conan2/remotes.json"
# --- opam (OCaml): default repo → artifact
mkdir -p "$H/.opam"
printf 'opam-version: "2.0"\nrepository: "%%s/artifacts/opam/"\n' "$A" > "$H/.opam/repo"
# --- git (CLI) — rewrite github to the artifact git mirror.
git config --global url."$A/artifacts/git/github.com/".insteadOf https://github.com/ 2>/dev/null || true
`, base, home)
}
