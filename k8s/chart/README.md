# workspace chart

Deploys the **workspace stack** as a self-contained Helm release, separate from
the standalone `abcp-agent` chart:

- `workspace-agent` — a dedicated agent (h2c) that loads the workspace role
  presets (`SYSTEM_PRESETS_FILE`), bootstraps the workspace tenant, and seeds
  the extensions' config into its own cfg KV.
- `workspace-extension` — `sandbox-*` / `repo-*` tools (NATS only, no Service).
- `workspace-playwright` — browser-automation extension (drives infra Selenium).
- `workspace-gateway` — the trusted session-creation + sandbox/service lifecycle
  surface (`workspace.v1`); manages objects in the `worker` namespace.
- `workspace-webui` — Caddy aggregator (static SPA + same-origin RPC).

Shared infrastructure is consumed over Service DNS from the separate `infra`
release (NATS, Garage S3, Forgejo, buildkitd, Selenium). Nothing infra-ish is
deployed here.

## Prerequisites

1. **The `infra` release** running, with the **`workspace` NATS account** added
   to `nats.conf` (see `agent/k8s/infra-chart`). The workspace agent must NOT
   share the standalone agent's NATS account: the two would collide on the
   global `abc.discover` subject, the `abc-presence` KV keys and the fixed
   `ABC_MAILBOX`/`ABC_EVENTS`/`ABC_DLQ` streams.
2. The **`worker` namespace exists** (this chart only creates the Role /
   RoleBinding inside it; it does not create the namespace).
3. Images built + pushed (agent inherits the abcp agent image; the rest are
   workspace images): `workspace-extension`, `workspace-gateway`,
   `workspace-webui`, and `playwright-extension`.

## Install

```sh
# The worker namespace must exist first.
kubectl create namespace worker

helm install workspace ./k8s/chart -n agent --set namespaceOverride=agent
```

## Before the first Helm install (one-time cleanup)

If the gateway was previously applied by hand (`kubectl apply -f
k8s/workspace-gateway.yaml`), delete those objects first so Helm can adopt the
names without an ownership conflict:

```sh
kubectl -n agent delete deploy workspace-gateway svc workspace-gateway \
  secret workspace-gateway-forgejo secret workspace-gateway-service --ignore-not-found
```

The cross-namespace RBAC (`workspace-gateway` SA + `worker` Role/RoleBinding)
is also recreated by this chart; the old hand-applied copies (if any) can be
deleted the same way.

## Namespacing / collisions

The chart is meant to run **in the same namespace as the standalone agent
chart**, so every object name is prefixed `workspace-` to stay unique:
`workspace-agent`, `workspace-extension`, `workspace-playwright`,
`workspace-gateway`, `workspace-webui`. In particular the agent chart's
`playwright-extension` is a DIFFERENT release from this chart's
`workspace-playwright`.
