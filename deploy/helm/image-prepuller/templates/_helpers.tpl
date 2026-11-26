{{/*
Common helpers for the image-prepuller chart. No external chart dependencies.
*/}}

{{/* Chart name, truncated to 63 chars. */}}
{{- define "ip.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Release fullname: <release>-<chart> unless overridden or already included. */}}
{{- define "ip.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/* chart:version label value. */}}
{{- define "ip.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Selector labels. Context: dict "ctx" (root) "component" (hub|spoke|agent).
Selector labels are immutable: never add release-version values here.
*/}}
{{- define "ip.selectorLabels" -}}
app.kubernetes.io/name: {{ include "ip.name" .ctx }}
app.kubernetes.io/instance: {{ .ctx.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end }}

{{/* Full labels for object metadata. Context: dict "ctx" "component". */}}
{{- define "ip.labels" -}}
helm.sh/chart: {{ include "ip.chart" .ctx }}
{{ include "ip.selectorLabels" . }}
app.kubernetes.io/version: {{ .ctx.Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .ctx.Release.Service }}
{{- end }}

{{/* Image reference; image.tag defaults to Chart.AppVersion. */}}
{{- define "ip.image" -}}
{{- printf "%s:%s" .Values.image.repository (.Values.image.tag | default .Chart.AppVersion) }}
{{- end }}

{{/* ServiceAccount name used by all workloads. */}}
{{- define "ip.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "ip.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* ServiceAccount name used by the agent DaemonSet (no API permissions). */}}
{{- define "ip.agentServiceAccountName" -}}
{{- if .Values.agent.serviceAccount.create }}
{{- default (printf "%s-agent" (include "ip.fullname" .)) .Values.agent.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.agent.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* ClusterRole/ClusterRoleBinding name for the hub controller. */}}
{{- define "ip.rbacName" -}}
{{- printf "%s-rbac" (include "ip.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Secret name holding auth-user/auth-pass/session-secret/cluster-token. */}}
{{- define "ip.secretName" -}}
{{- if .Values.secret.existingSecret }}
{{- .Values.secret.existingSecret }}
{{- else }}
{{- printf "%s-secret" (include "ip.fullname" .) }}
{{- end }}
{{- end }}

{{/* Rendered pod annotations: podAnnotations + optional metrics scrape. */}}
{{- define "ip.podAnnotations" -}}
{{- $ctx := .ctx }}
{{- with $ctx.Values.podAnnotations }}
{{ toYaml . }}
{{- end }}
{{- if .metrics }}
prometheus.io/scrape: "true"
prometheus.io/port: {{ .metricsPort | quote }}
prometheus.io/path: "/metrics"
{{- end }}
{{- end }}
