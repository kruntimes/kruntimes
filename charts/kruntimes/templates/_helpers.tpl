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

{{- define "kruntimes.gateway.name" -}}
{{- printf "%s-gateway" ((include "kruntimes.fullname" .) | trunc 56 | trimSuffix "-") }}
{{- end }}

{{- define "kruntimes.gateway.validateProtocols" -}}
{{- if eq (len .Values.gateway.protocols) 0 -}}{{- fail "gateway.protocols must include http or https" -}}{{- end -}}
{{- $seen := dict -}}
{{- range $protocol := .Values.gateway.protocols -}}
{{- if not (or (eq $protocol "http") (eq $protocol "https")) -}}{{- fail "gateway.protocols values must be http or https" -}}{{- end -}}
{{- if hasKey $seen $protocol -}}{{- fail "gateway.protocols values must be distinct" -}}{{- end -}}
{{- $_ := set $seen $protocol true -}}
{{- end -}}
{{- end }}
{{- define "kruntimes.gateway.validateTransferBounds" -}}
{{- if le (int64 .Values.gateway.maxRequestBodyBytes) 0 -}}{{- fail "gateway.maxRequestBodyBytes must be positive" -}}{{- end -}}
{{- if le (int64 .Values.gateway.maxResponseBodyBytes) 0 -}}{{- fail "gateway.maxResponseBodyBytes must be positive" -}}{{- end -}}
{{- if le (int64 .Values.gateway.maxHeaderBytes) 0 -}}{{- fail "gateway.maxHeaderBytes must be positive" -}}{{- end -}}
{{- end }}
{{- define "kruntimes.gateway.tlsSecretName" -}}
{{- default (printf "%s-tls" (include "kruntimes.gateway.name" .)) .Values.gateway.tls.secretName -}}
{{- end }}
{{- define "kruntimes.gateway.ensureTLSCertificates" -}}
{{- if not (hasKey .Values "_gatewayCertificates") -}}
{{- $secretName := include "kruntimes.gateway.tlsSecretName" . -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace $secretName -}}
{{- if $existing -}}
{{- if not (hasKey $existing.data .Values.gateway.tls.caBundleKey) -}}{{- fail (printf "gateway TLS Secret %q must contain CA bundle key %q" $secretName .Values.gateway.tls.caBundleKey) -}}{{- end -}}
{{- if not (hasKey $existing.data .Values.gateway.tls.certificateKey) -}}{{- fail (printf "gateway TLS Secret %q must contain certificate key %q" $secretName .Values.gateway.tls.certificateKey) -}}{{- end -}}
{{- if not (hasKey $existing.data .Values.gateway.tls.privateKeyKey) -}}{{- fail (printf "gateway TLS Secret %q must contain private key key %q" $secretName .Values.gateway.tls.privateKeyKey) -}}{{- end -}}
{{- $_ := set .Values "_gatewayCertificates" (dict "caCert" (index $existing.data .Values.gateway.tls.caBundleKey | b64dec) "tlsCert" (index $existing.data .Values.gateway.tls.certificateKey | b64dec) "tlsKey" (index $existing.data .Values.gateway.tls.privateKeyKey | b64dec)) -}}
{{- else if not .Values.gateway.tls.secretName -}}
{{- $ca := genCA (printf "%s-ca" (include "kruntimes.gateway.name" .)) 3650 -}}
{{- $name := include "kruntimes.gateway.name" . -}}
{{- $dns := list $name (printf "%s.%s" $name .Release.Namespace) (printf "%s.%s.svc" $name .Release.Namespace) (printf "%s.%s.svc.cluster.local" $name .Release.Namespace) -}}
{{- $certificate := genSignedCert $name nil $dns 365 $ca -}}
{{- $_ := set .Values "_gatewayCertificates" (dict "caCert" $ca.Cert "tlsCert" $certificate.Cert "tlsKey" $certificate.Key) -}}
{{- else -}}
{{- fail (printf "gateway TLS Secret %q was not found" $secretName) -}}
{{- end -}}
{{- end -}}
{{- end }}
{{- define "kruntimes.gateway.validateTLS" -}}
{{- if has "https" .Values.gateway.protocols -}}
{{- if not .Values.gateway.tls.certificateKey -}}
{{- fail "gateway.tls.certificateKey is required when gateway.protocols includes https" -}}
{{- end -}}
{{- if not .Values.gateway.tls.privateKeyKey -}}
{{- fail "gateway.tls.privateKeyKey is required when gateway.protocols includes https" -}}
{{- end -}}
{{- if .Values.gateway.tls.certManager.enabled -}}
{{- if not .Values.gateway.tls.secretName -}}
{{- fail "gateway.tls.secretName is required when gateway.tls.certManager.enabled is true" -}}
{{- end -}}
{{- if not .Values.gateway.tls.certManager.issuerRef.name -}}
{{- fail "gateway.tls.certManager.issuerRef.name is required when gateway.tls.certManager.enabled is true" -}}
{{- end -}}
{{- end -}}
{{- if and .Values.gateway.tls.clientCASecretName (not .Values.gateway.tls.clientCAKey) -}}
{{- fail "gateway.tls.clientCAKey is required when gateway.tls.clientCASecretName is set" -}}
{{- end -}}
{{- end -}}
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

{{- define "kruntimes.gateway.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kruntimes.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: runtime-gateway
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

{{- define "kruntimes.gateway.labels" -}}
{{ include "kruntimes.labels" . }}
app.kubernetes.io/component: runtime-gateway
app: kruntimes-runtime-gateway
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

{{- define "kruntimes.dashboard.name" -}}
{{- printf "%s-dashboard" ((include "kruntimes.fullname" .) | trunc 54 | trimSuffix "-") -}}
{{- end }}

{{- define "kruntimes.dashboard.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kruntimes.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: dashboard
{{- end }}

{{- define "kruntimes.dashboard.labels" -}}
{{ include "kruntimes.labels" . }}
app.kubernetes.io/component: dashboard
app: kruntimes-dashboard
{{- end }}

{{- define "kruntimes.dashboard.tlsSecretName" -}}
{{- default (printf "%s-tls" (include "kruntimes.dashboard.name" .)) .Values.dashboard.tls.secretName -}}
{{- end }}

{{- define "kruntimes.dashboard.validateTLS" -}}
{{- if not .Values.dashboard.tls.certificateKey -}}{{- fail "dashboard.tls.certificateKey is required" -}}{{- end -}}
{{- if not .Values.dashboard.tls.privateKeyKey -}}{{- fail "dashboard.tls.privateKeyKey is required" -}}{{- end -}}
{{- if and .Values.dashboard.tls.selfSigned .Values.dashboard.tls.certManager.enabled -}}{{- fail "dashboard.tls.selfSigned and dashboard.tls.certManager.enabled are mutually exclusive" -}}{{- end -}}
{{- if and (not .Values.dashboard.tls.selfSigned) (not .Values.dashboard.tls.certManager.enabled) (not .Values.dashboard.tls.secretName) -}}{{- fail "dashboard.tls.secretName is required when using an existing TLS Secret" -}}{{- end -}}
{{- if and .Values.dashboard.tls.certManager.enabled (not .Values.dashboard.tls.certManager.issuerRef.name) -}}{{- fail "dashboard.tls.certManager.issuerRef.name is required when dashboard.tls.certManager.enabled is true" -}}{{- end -}}
{{- end }}

{{- define "kruntimes.dashboard.ensureTLSCertificates" -}}
{{- if not (hasKey .Values "_dashboardCertificates") -}}
{{- $secretName := include "kruntimes.dashboard.tlsSecretName" . -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace $secretName -}}
{{- if $existing -}}
{{- if not (hasKey $existing.data .Values.dashboard.tls.certificateKey) -}}{{- fail (printf "dashboard TLS Secret %q must contain certificate key %q" $secretName .Values.dashboard.tls.certificateKey) -}}{{- end -}}
{{- if not (hasKey $existing.data .Values.dashboard.tls.privateKeyKey) -}}{{- fail (printf "dashboard TLS Secret %q must contain private key key %q" $secretName .Values.dashboard.tls.privateKeyKey) -}}{{- end -}}
{{- $_ := set .Values "_dashboardCertificates" (dict "tlsCert" (index $existing.data .Values.dashboard.tls.certificateKey | b64dec) "tlsKey" (index $existing.data .Values.dashboard.tls.privateKeyKey | b64dec)) -}}
{{- else -}}
{{- $name := include "kruntimes.dashboard.name" . -}}
{{- $dns := list $name (printf "%s.%s" $name .Release.Namespace) (printf "%s.%s.svc" $name .Release.Namespace) (printf "%s.%s.svc.cluster.local" $name .Release.Namespace) -}}
{{- $ca := genCA (printf "%s-ca" $name) 3650 -}}
{{- $certificate := genSignedCert $name nil $dns 365 $ca -}}
{{- $_ := set .Values "_dashboardCertificates" (dict "tlsCert" $certificate.Cert "tlsKey" $certificate.Key) -}}
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
