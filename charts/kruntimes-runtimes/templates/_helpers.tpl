{{- define "kruntimes-runtimes.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "kruntimes-runtimes.image" -}}
{{- $root := index . 0 -}}
{{- $image := index . 1 -}}
{{- if or (contains "@" $image) (regexMatch "(^|/)[^/]+:[^/]+$" $image) -}}
{{- $image -}}
{{- else -}}
{{- printf "%s:%s" $image $root.Chart.AppVersion -}}
{{- end -}}
{{- end }}
