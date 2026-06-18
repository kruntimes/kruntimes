{{- define "kruntimes.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "kruntimes.fullname" -}}
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

{{- define "kruntimes.labels" -}}
app.kubernetes.io/name: {{ include "kruntimes.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "kruntimes.controller.name" -}}
{{- printf "%s-controller" ((include "kruntimes.fullname" .) | trunc 52 | trimSuffix "-") }}
{{- end }}

{{- define "kruntimes.scheduler.name" -}}
{{- printf "%s-scheduler" ((include "kruntimes.fullname" .) | trunc 53 | trimSuffix "-") }}
{{- end }}

{{- define "kruntimes.runtimed.name" -}}
{{- printf "%s-runtimed" ((include "kruntimes.fullname" .) | trunc 54 | trimSuffix "-") }}
{{- end }}

{{- define "kruntimes.controller.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kruntimes.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: controller
{{- end }}

{{- define "kruntimes.scheduler.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kruntimes.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: scheduler
{{- end }}

{{- define "kruntimes.controller.labels" -}}
{{ include "kruntimes.labels" . }}
app.kubernetes.io/component: controller
app: kruntimes-controller
{{- end }}

{{- define "kruntimes.scheduler.labels" -}}
{{ include "kruntimes.labels" . }}
app.kubernetes.io/component: scheduler
app: kruntimes-scheduler
{{- end }}

{{- define "kruntimes.runtimed.labels" -}}
{{ include "kruntimes.labels" . }}
app.kubernetes.io/component: runtimed
app: kruntimes-runtimed
{{- end }}

{{- define "kruntimes.image" -}}
{{- $root := index . 0 -}}
{{- $image := index . 1 -}}
{{- if or (contains "@" $image) (regexMatch "(^|/)[^/]+:[^/]+$" $image) -}}
{{- $image -}}
{{- else -}}
{{- printf "%s:%s" $image $root.Chart.AppVersion -}}
{{- end -}}
{{- end }}
