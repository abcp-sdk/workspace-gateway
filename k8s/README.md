# k8s/ — moved

The workspace stack's deployment assets (the `workspace` Helm chart, the
`workspace-local` StorageClass provisioner, and the cross-namespace RBAC /
legacy manifests) live in the dedicated deploy repo:

**`coding-workspace/deploy`**

```
charts/workspace/     the Helm chart (release "workspace")
provisioner/          workspace-local StorageClass
manifests/            rbac.yaml, workspace-gateway.yaml (legacy)
```

Install from there:

```sh
helm install workspace ./charts/workspace -n agent --set namespaceOverride=agent
```

Do not re-add charts here — the deploy repo is the single source of truth for
the workspace stack's Kubernetes objects.
