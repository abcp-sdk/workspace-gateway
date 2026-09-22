{{/* Namespace: release namespace unless overridden. */}}
{{- define "workspace.namespace" -}}
{{- .Values.namespaceOverride | default .Release.Namespace -}}
{{- end -}}

{{- define "workspace.labels" -}}
app.kubernetes.io/name: workspace
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end -}}

{{- define "workspace.saName" -}}
{{- if .Values.serviceAccount.create -}}
{{- .Values.serviceAccount.name | default "workspace-gateway" -}}
{{- else -}}
default
{{- end -}}
{{- end -}}

{{/* In-cluster h2c agent URL (the workspace agent this chart deploys). */}}
{{- define "workspace.agentUrl" -}}
{{- printf "http://%s.%s.svc.cluster.local:80" .Values.agent.service.name (include "workspace.namespace" .) -}}
{{- end -}}

{{/* NATS URL for the dedicated `workspace` account. */}}
{{- define "workspace.natsUrl" -}}
{{- printf "nats://%s:%s@%s:%v" .Values.infra.nats.user .Values.infra.nats.password .Values.infra.nats.host .Values.infra.nats.port -}}
{{- end -}}

{{/* Gateway Service URL (used by the webui Caddy aggregator). */}}
{{- define "workspace.gatewayUrl" -}}
{{- printf "%s.%s.svc.cluster.local:80" "workspace-gateway" (include "workspace.namespace" .) -}}
{{- end -}}

{{/* S3 object-store env (durable file bytes). */}}
{{- define "workspace.objectStoreEnv" -}}
- name: AGENT_BLOB_BACKEND
  value: "s3"
- name: S3_BUCKET
  value: {{ .Values.infra.s3.bucket | quote }}
- name: S3_REGION
  value: {{ .Values.infra.s3.region | quote }}
- name: S3_ENDPOINT
  value: {{ .Values.infra.s3.endpoint | quote }}
- name: S3_ACCESS_KEY
  value: {{ .Values.infra.s3.accessKey | quote }}
- name: S3_SECRET_KEY
  value: {{ .Values.infra.s3.secretKey | quote }}
- name: S3_PATH_STYLE
  value: {{ .Values.infra.s3.pathStyle | toString | quote }}
- name: S3_PREFIX
  value: {{ .Values.infra.s3.prefix | quote }}
{{- end -}}

{{/* no_proxy must cover in-cluster DNS + the registries: the namespace injects
     http_proxy but NOT no_proxy, so without this in-cluster calls get proxied. */}}
{{- define "workspace.noProxy" -}}
localhost,127.0.0.1,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,.svc.cluster.local,.svc,.fenjin.org,.nip.io,10.199.64.20
{{- end -}}

{{/* Outbound egress proxy env for a Node process. `NODE_USE_ENV_PROXY=1` makes
     Node's built-in fetch honor HTTP(S)_PROXY (Node 24+); the bundled
     web-fetch / brave-search tools run IN the agent process and rely on it. */}}
{{- define "workspace.egressProxyEnv" -}}
- name: NODE_USE_ENV_PROXY
  value: "1"
- name: http_proxy
  value: {{ .Values.infra.proxy.http | quote }}
- name: https_proxy
  value: {{ .Values.infra.proxy.https | quote }}
- name: HTTP_PROXY
  value: {{ .Values.infra.proxy.http | quote }}
- name: HTTPS_PROXY
  value: {{ .Values.infra.proxy.https | quote }}
- name: no_proxy
  value: {{ include "workspace.noProxy" . | quote }}
- name: NO_PROXY
  value: {{ include "workspace.noProxy" . | quote }}
{{- end -}}
