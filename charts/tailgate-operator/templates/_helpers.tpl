{{/*
Expand the name of the chart.
*/}}
{{- define "tailgate-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "tailgate-operator.fullname" -}}
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

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "tailgate-operator.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "tailgate-operator.labels" -}}
helm.sh/chart: {{ include "tailgate-operator.chart" . }}
{{ include "tailgate-operator.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels (chart-wide). Per-workload selectors add an app.kubernetes.io/component.
*/}}
{{- define "tailgate-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "tailgate-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Operator selector labels
*/}}
{{- define "tailgate-operator.operator.selectorLabels" -}}
{{ include "tailgate-operator.selectorLabels" . }}
app.kubernetes.io/component: operator
{{- end }}

{{/*
Agent selector labels
*/}}
{{- define "tailgate-operator.agent.selectorLabels" -}}
{{ include "tailgate-operator.selectorLabels" . }}
app.kubernetes.io/component: agent
{{- end }}

{{/*
Agent DaemonSet name. The agent is a sibling component of the operator (it is not part of
it), so it is named "tailgate-agent" rather than "<operator-fullname>-agent". Override with
agent.nameOverride.
*/}}
{{- define "tailgate-operator.agentName" -}}
{{- default "tailgate-agent" .Values.agent.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "tailgate-operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "tailgate-operator.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Create the name for the manager ClusterRole
*/}}
{{- define "tailgate-operator.clusterRoleName" -}}
{{- printf "%s-manager-role" (include "tailgate-operator.fullname" .) }}
{{- end }}

{{/*
Operator container image
*/}}
{{- define "tailgate-operator.operator.image" -}}
{{- $tag := default .Chart.AppVersion .Values.operator.image.tag }}
{{- printf "%s:%s" .Values.operator.image.repository $tag }}
{{- end }}

{{/*
Agent container image
*/}}
{{- define "tailgate-operator.agent.image" -}}
{{- $tag := default .Chart.AppVersion .Values.agent.image.tag }}
{{- printf "%s:%s" .Values.agent.image.repository $tag }}
{{- end }}

{{- /* Gateway container image stamped into per-group gateway workloads */ -}}
{{- define "tailgate-operator.gateway.image" -}}
{{- $tag := default .Chart.AppVersion .Values.gateway.image.tag }}
{{- printf "%s:%s" .Values.gateway.image.repository $tag }}
{{- end }}

{{- /* UI container image */ -}}
{{- define "tailgate-operator.ui.image" -}}
{{- $tag := default .Chart.AppVersion .Values.ui.image.tag }}
{{- printf "%s:%s" .Values.ui.image.repository $tag }}
{{- end }}
