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

{{- define "kruntimes.webhook.name" -}}
{{- printf "%s-webhook" ((include "kruntimes.controller.name" .) | trunc 55 | trimSuffix "-") }}
{{- end }}

{{- define "kruntimes.webhook.secretName" -}}
{{- printf "%s-tls" (include "kruntimes.webhook.name" .) }}
{{- end }}

{{- define "kruntimes.webhook.ensureCertificates" -}}
{{- if not (hasKey .Values "_webhookCertificates") -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace (include "kruntimes.webhook.secretName" .) -}}
{{- if $existing -}}
{{- $_ := set .Values "_webhookCertificates" (dict "caCert" (index $existing.data "ca.crt" | b64dec) "caKey" (index $existing.data "ca.key" | b64dec) "tlsCert" (index $existing.data "tls.crt" | b64dec) "tlsKey" (index $existing.data "tls.key" | b64dec)) -}}
{{- else -}}
{{- $ca := genCA (printf "%s-ca" (include "kruntimes.webhook.name" .)) 3650 -}}
{{- $serviceName := include "kruntimes.webhook.name" . -}}
{{- $dnsNames := list $serviceName (printf "%s.%s" $serviceName .Release.Namespace) (printf "%s.%s.svc" $serviceName .Release.Namespace) -}}
{{- $certificate := genSignedCert $serviceName nil $dnsNames 365 $ca -}}
{{- $_ := set .Values "_webhookCertificates" (dict "caCert" $ca.Cert "caKey" $ca.Key "tlsCert" $certificate.Cert "tlsKey" $certificate.Key) -}}
{{- end -}}
{{- end -}}
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

{{- define "kruntimes.logAPI.name" -}}
{{- printf "%s-log-api" ((include "kruntimes.fullname" .) | trunc 55 | trimSuffix "-") -}}
{{- end }}
{{- define "kruntimes.logAPI.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kruntimes.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: runtime-log-api
{{- end }}
{{- define "kruntimes.logAPI.labels" -}}
{{ include "kruntimes.labels" . }}
app.kubernetes.io/component: runtime-log-api
app: kruntimes-runtime-log-api
{{- end }}
{{- define "kruntimes.logAPI.tlsSecretName" -}}
{{- default (printf "%s-tls" (include "kruntimes.logAPI.name" .)) .Values.logAPI.tls.secretName -}}
{{- end }}
{{- define "kruntimes.logAPI.ensureTLSCertificates" -}}
{{- if not (hasKey .Values "_logAPICertificates") -}}
{{- $secretName := include "kruntimes.logAPI.tlsSecretName" . -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace $secretName -}}
{{- if $existing -}}
{{- if not (hasKey $existing.data .Values.logAPI.tls.caBundleKey) -}}{{- fail (printf "logAPI TLS Secret %q must contain CA bundle key %q" $secretName .Values.logAPI.tls.caBundleKey) -}}{{- end -}}
{{- if not (hasKey $existing.data .Values.logAPI.tls.certificateKey) -}}{{- fail (printf "logAPI TLS Secret %q must contain certificate key %q" $secretName .Values.logAPI.tls.certificateKey) -}}{{- end -}}
{{- if not (hasKey $existing.data .Values.logAPI.tls.privateKeyKey) -}}{{- fail (printf "logAPI TLS Secret %q must contain private key key %q" $secretName .Values.logAPI.tls.privateKeyKey) -}}{{- end -}}
{{- $_ := set .Values "_logAPICertificates" (dict "caCert" (index $existing.data .Values.logAPI.tls.caBundleKey | b64dec) "tlsCert" (index $existing.data .Values.logAPI.tls.certificateKey | b64dec) "tlsKey" (index $existing.data .Values.logAPI.tls.privateKeyKey | b64dec)) -}}
{{- else if not .Values.logAPI.tls.secretName -}}
{{- $name := include "kruntimes.logAPI.name" . -}}
{{- $dns := list $name (printf "%s.%s" $name .Release.Namespace) (printf "%s.%s.svc" $name .Release.Namespace) (printf "%s.%s.svc.cluster.local" $name .Release.Namespace) -}}
{{- $ca := genCA (printf "%s-ca" $name) 3650 -}}
{{- $certificate := genSignedCert $name nil $dns 365 $ca -}}
{{- $_ := set .Values "_logAPICertificates" (dict "caCert" $ca.Cert "tlsCert" $certificate.Cert "tlsKey" $certificate.Key) -}}
{{- else -}}{{- fail (printf "logAPI TLS Secret %q was not found" $secretName) -}}{{- end -}}
{{- end -}}
{{- end }}

{{- define "kruntimes.console.name" -}}
{{- printf "%s-console" ((include "kruntimes.fullname" .) | trunc 56 | trimSuffix "-") -}}
{{- end }}

{{- define "kruntimes.console.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kruntimes.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: console
{{- end }}

{{- define "kruntimes.console.labels" -}}
{{ include "kruntimes.labels" . }}
app.kubernetes.io/component: console
app: kruntimes-console
{{- end }}

{{- define "kruntimes.console.tlsSecretName" -}}
{{- default (printf "%s-tls" (include "kruntimes.console.name" .)) .Values.console.tls.secretName -}}
{{- end }}

{{- define "kruntimes.console.validateTLS" -}}
{{- if not .Values.console.tls.certificateKey -}}{{- fail "console.tls.certificateKey is required" -}}{{- end -}}
{{- if not .Values.console.tls.privateKeyKey -}}{{- fail "console.tls.privateKeyKey is required" -}}{{- end -}}
{{- if not .Values.console.tls.caBundleKey -}}{{- fail "console.tls.caBundleKey is required" -}}{{- end -}}
{{- if le (int64 .Values.console.access.maxRequestBodyBytes) 0 -}}{{- fail "console.access.maxRequestBodyBytes must be positive" -}}{{- end -}}
{{- if le (int64 .Values.console.access.maxResponseBodyBytes) 0 -}}{{- fail "console.access.maxResponseBodyBytes must be positive" -}}{{- end -}}
{{- if le (int64 .Values.console.access.maxHeaderBytes) 0 -}}{{- fail "console.access.maxHeaderBytes must be positive" -}}{{- end -}}
{{- if and .Values.console.tls.selfSigned .Values.console.tls.certManager.enabled -}}{{- fail "console.tls.selfSigned and console.tls.certManager.enabled are mutually exclusive" -}}{{- end -}}
{{- if and (not .Values.console.tls.selfSigned) (not .Values.console.tls.certManager.enabled) (not .Values.console.tls.secretName) -}}{{- fail "console.tls.secretName is required when using an existing TLS Secret" -}}{{- end -}}
{{- if and .Values.console.tls.certManager.enabled (not .Values.console.tls.certManager.issuerRef.name) -}}{{- fail "console.tls.certManager.issuerRef.name is required when console.tls.certManager.enabled is true" -}}{{- end -}}
{{- if and .Values.console.tls.clientCASecretName (not .Values.console.tls.clientCAKey) -}}{{- fail "console.tls.clientCAKey is required when console.tls.clientCASecretName is set" -}}{{- end -}}
{{- end }}

{{- define "kruntimes.console.ensureTLSCertificates" -}}
{{- if not (hasKey .Values "_consoleCertificates") -}}
{{- $secretName := include "kruntimes.console.tlsSecretName" . -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace $secretName -}}
{{- if $existing -}}
{{- if not (hasKey $existing.data .Values.console.tls.caBundleKey) -}}{{- fail (printf "console TLS Secret %q must contain CA bundle key %q" $secretName .Values.console.tls.caBundleKey) -}}{{- end -}}
{{- if not (hasKey $existing.data .Values.console.tls.certificateKey) -}}{{- fail (printf "console TLS Secret %q must contain certificate key %q" $secretName .Values.console.tls.certificateKey) -}}{{- end -}}
{{- if not (hasKey $existing.data .Values.console.tls.privateKeyKey) -}}{{- fail (printf "console TLS Secret %q must contain private key key %q" $secretName .Values.console.tls.privateKeyKey) -}}{{- end -}}
{{- $_ := set .Values "_consoleCertificates" (dict "caCert" (index $existing.data .Values.console.tls.caBundleKey | b64dec) "tlsCert" (index $existing.data .Values.console.tls.certificateKey | b64dec) "tlsKey" (index $existing.data .Values.console.tls.privateKeyKey | b64dec)) -}}
{{- else -}}
{{- $name := include "kruntimes.console.name" . -}}
{{- $dns := list $name (printf "%s.%s" $name .Release.Namespace) (printf "%s.%s.svc" $name .Release.Namespace) (printf "%s.%s.svc.cluster.local" $name .Release.Namespace) -}}
{{- $ca := genCA (printf "%s-ca" $name) 3650 -}}
{{- $certificate := genSignedCert $name nil $dns 365 $ca -}}
{{- $_ := set .Values "_consoleCertificates" (dict "caCert" $ca.Cert "tlsCert" $certificate.Cert "tlsKey" $certificate.Key) -}}
{{- end -}}
{{- end -}}
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
