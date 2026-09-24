# Sandbox images

Sandboxes run a **pre-built, worker-bundled** image. The gateway no longer
injects the worker at launch: `CreateSandbox` refuses any image whose registry
org is not `SANDBOX_ORG` (default `sandbox`).

`build.sh` bakes the agent-worker binary (cross-compiled from `../worker-src`)
into an existing GENERIC toolchain image and pushes it to
`<registry>/<SANDBOX_ORG>/sandbox-<lang>:<distro-tag>`:

    ./build.sh                       # every toolchain (the default set)
    ./build.sh base node python      # a subset
    ./build.sh base                  # just the base

The default set mirrors the `agent-toolchain` catalog (17 images):
`base node python go rust java java25 kotlin scala clang dotnet ruby php
elixir dart swift zig` → `sandbox-<lang>:debian-trixie`.

Re-run whenever the worker changes (a worker fix requires rebuilt images).
