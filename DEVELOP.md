# Developing workspace-gateway

## Build & test

```bash
go build ./...
go test ./...
go run ./cmd/gen-presets > presets/system-presets.json   # regenerate system presets
./build-image.sh                                          # build+push the image (buildkitd)
```

The generated protos are vendored under `gen/`; regenerate them with the
`proto/buf.gen.*.yaml` templates (`buf generate`) rather than editing `gen/`.

## Packages via artifact (not GitHub/public registries)

- Consume: `GOPROXY=http://artifact.worker.svc.cluster.local/artifacts/go GOSUMDB=off`
- Publish: `curl -X PUT --data-binary @<zip> "http://artifact.worker.svc.cluster.local/artifacts/go/upload?name=<module>&version=vX.Y.Z"`

See `easy-vcs/deploy:PUBLISHING.md`.

## Sandbox notes

The dev sandbox image has **no Go toolchain preinstalled**. A matching one can be
fetched from the public CDN (direct egress works):

```bash
curl -fsSL -o /tmp/go.tgz https://go.dev/dl/go1.27.1.linux-amd64.tar.gz
rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz
```

`proxy.golang.org` is **not reachable** from the sandbox; fetch modules from the
platform's private **artifact** proxy (see above) and skip the checksum DB:

```bash
go env -w GOPROXY=http://artifact.worker.svc.cluster.local/artifacts/go GOSUMDB=off GOTOOLCHAIN=local
go mod download all
```

`GOTOOLCHAIN=local` matters: `go.mod` targets go 1.26 and a toolchain switch would
try (and fail) to fetch another toolchain.

## Helm release state

`internal/helmmgr` stores release state as **one ConfigMap per revision** plus a
small **head** record per release (`helm-release-<release>`), mirroring upstream
Helm's storage driver. Do NOT fold the history back into a single object: a chart
manifest is tens of KB, so ~30 revisions blow past the 1 MiB ConfigMap limit and
then **every** upgrade of that release fails (`ConfigMap ... Too long`), forcing a
destructive uninstall/reinstall.

* Head: `helm-release-<release>` (label `workspace/helm-record=head`) — metadata +
  the stored revision numbers; no manifests, so it stays tiny.
* Body: `helm-release-rev-<release>-v<N>` (label `workspace/helm-record=revision`)
  — that revision's ref/chart/values/objects/manifest.
* Both carry `workspace/helm-release=<release>` so one watch (`Client.Watch`)
  still covers the whole release; `Client.List` filters bodies out.
* `Client.Get` materializes `History` from the bodies and migrates a pre-split
  release (history embedded in the head) on read.
* `historyMax` (`HELM_HISTORY_MAX`, default 10) prunes older revisions after each
  write, so `HelmRollback` can only target the retained window.
