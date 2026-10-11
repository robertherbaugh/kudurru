{{/* Names */}}
{{- define "kudurru.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kudurru.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* Common labels; the component label is added per workload. */}}
{{- define "kudurru.commonLabels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/name: {{ include "kudurru.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: kudurru
{{- end -}}

{{- define "kudurru.labels" -}}
{{ include "kudurru.commonLabels" . }}
app.kubernetes.io/component: dns
{{- end -}}

{{- define "kudurru.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kudurru.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: dns
{{- end -}}

{{- define "kudurru.edge.labels" -}}
{{ include "kudurru.commonLabels" . }}
app.kubernetes.io/component: edge
{{- end -}}

{{- define "kudurru.edge.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kudurru.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: edge
{{- end -}}

{{- define "kudurru.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "kudurru.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "kudurru.headlessName" -}}
{{- default (printf "%s-headless" (include "kudurru.fullname" .)) .Values.service.headless.name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* KUDURRU_HEADLESS_SERVICE: serve.go dials <pod>.<this>:8082 for the leader. */}}
{{- define "kudurru.headlessFQDN" -}}
{{- printf "%s.%s.svc.%s" (include "kudurru.headlessName" .) .Release.Namespace (.Values.clusterDomain | default "cluster.local") -}}
{{- end -}}

{{/* Images */}}
{{- define "kudurru.image" -}}
{{- if .Values.image.digest -}}
{{ .Values.image.repository }}@{{ .Values.image.digest }}
{{- else -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end -}}
{{- end -}}

{{- define "kudurru.edge.image" -}}
{{- $img := .Values.edge.image -}}
{{- if $img.digest -}}
{{ $img.repository }}@{{ $img.digest }}
{{- else -}}
{{ $img.repository }}:{{ $img.tag | default (printf "%s-k%s" .Values.edge.dnsdistVersion .Chart.AppVersion) }}
{{- end -}}
{{- end -}}

{{- define "kudurru.keepalived.image" -}}
{{- $img := .Values.vip.keepalived.image -}}
{{- if $img.digest -}}
{{ $img.repository }}@{{ $img.digest }}
{{- else -}}
{{ $img.repository }}:{{ $img.tag | default (printf "%s-k%s" .Values.vip.keepalived.keepalivedVersion .Chart.AppVersion) }}
{{- end -}}
{{- end -}}

{{/* Topology switches */}}
{{- define "kudurru.standalone" -}}
{{- if eq .Values.mode "standalone" }}true{{ end -}}
{{- end -}}

{{/* Effective edge mode: keepalived forces the hostNetwork DaemonSet. */}}
{{- define "kudurru.edge.mode" -}}
{{- if .Values.edge.enabled -}}
{{- if eq .Values.vip.provider "keepalived" }}daemonset{{ else }}{{ .Values.edge.mode }}{{ end -}}
{{- end -}}
{{- end -}}

{{/* "true" when a separate edge workload (not the sidecar) fronts kudurru. */}}
{{- define "kudurru.edge.separate" -}}
{{- $m := include "kudurru.edge.mode" . -}}
{{- if or (eq $m "deployment") (eq $m "daemonset") }}true{{ end -}}
{{- end -}}

{{/* Workload kind of the kudurru pods. */}}
{{- define "kudurru.workloadKind" -}}
{{- if include "kudurru.standalone" . -}}
StatefulSet
{{- else if .Values.hostNetwork.enabled -}}
DaemonSet
{{- else if or .Values.persistence.querylog.enabled .Values.persistence.local.enabled -}}
StatefulSet
{{- else -}}
Deployment
{{- end -}}
{{- end -}}

{{- define "kudurru.replicas" -}}
{{- if include "kudurru.standalone" . }}1{{ else }}{{ .Values.replicaCount }}{{ end -}}
{{- end -}}

{{/* Port kudurru's plain DNS listener binds. */}}
{{- define "kudurru.dnsPort" -}}
{{- if .Values.edge.enabled }}{{ .Values.edge.backendPort }}{{ else }}{{ .Values.listen.port }}{{ end -}}
{{- end -}}

{{/* Encrypted transports enabled on the Service. */}}
{{- define "kudurru.anyTLS" -}}
{{- $s := .Values.service -}}
{{- if or $s.dot.enabled $s.doh.enabled $s.doq.enabled $s.doh3.enabled }}true{{ end -}}
{{- end -}}

{{/* Secret names */}}
{{- define "kudurru.encryptionKeySecret" -}}
{{- default (printf "%s-encryption-key" (include "kudurru.fullname" .)) .Values.auth.encryptionKeySecret -}}
{{- end -}}

{{- define "kudurru.adminSecret" -}}
{{- default (printf "%s-admin" (include "kudurru.fullname" .)) .Values.auth.bootstrapAdminSecret -}}
{{- end -}}

{{- define "kudurru.bootstrapTokenSecret" -}}
{{- default (printf "%s-bootstrap-token" (include "kudurru.fullname" .)) .Values.auth.bootstrapToken.existingSecret -}}
{{- end -}}

{{- define "kudurru.proxySecret" -}}
{{- default (printf "%s-proxy-protocol" (include "kudurru.fullname" .)) .Values.edge.existingSecret -}}
{{- end -}}

{{- define "kudurru.vrrpSecret" -}}
{{- default (printf "%s-vrrp" (include "kudurru.fullname" .)) .Values.vip.keepalived.existingSecret -}}
{{- end -}}

{{/*
Returns the base64 value of key in an existing Secret, or "" (helm template
and first install). Generated secrets are reused on upgrade through this.
*/}}
{{- define "kudurru.lookupSecretValue" -}}
{{- $s := lookup "v1" "Secret" .ns .name -}}
{{- if and $s $s.data -}}
{{- index $s.data .key | default "" -}}
{{- end -}}
{{- end -}}

{{/*
The node file (/etc/kudurru/node/kudurru.yaml). Every key here exists in
internal/config/schema.go; the deploy tests load the rendered file through
config.Load for every values permutation. The chart's listener layout comes
first and config.node is merged over it. config.settings is not merged here:
the binary reads it from the mounted settings file (KUDURRU_SETTINGS_FILE,
D106) and hot-reloads it, so settings changes do not roll the pods.
*/}}
{{- define "kudurru.nodeConfig" -}}
{{- $standalone := include "kudurru.standalone" . -}}
{{- $edgeMode := include "kudurru.edge.mode" . -}}
{{- $s := .Values.service -}}
{{- $dnsAddr := printf "0.0.0.0:%d" (int (include "kudurru.dnsPort" .)) -}}
{{- if eq $edgeMode "sidecar" -}}
{{- $dnsAddr = printf "127.0.0.1:%d" (int .Values.edge.backendPort) -}}
{{- end -}}
{{- $listeners := dict
    "dns" (list (dict "address" $dnsAddr "protocols" (list "udp" "tcp")))
    "api" (dict "address" "0.0.0.0:8080")
    "ops" (dict "address" "0.0.0.0:8081")
    "metrics" (dict "address" "0.0.0.0:9153")
    "internal" (dict "address" "0.0.0.0:8082") -}}
{{- if and (not .Values.edge.enabled) (include "kudurru.anyTLS" .) -}}
{{- $tls := dict "certFile" "/etc/kudurru/tls/tls.crt" "keyFile" "/etc/kudurru/tls/tls.key" -}}
{{- if $s.dot.enabled }}{{ $_ := set $listeners "dot" (list (dict "address" (printf "0.0.0.0:%d" (int $s.dot.port)) "tls" $tls)) }}{{ end -}}
{{- if $s.doq.enabled }}{{ $_ := set $listeners "doq" (list (dict "address" (printf "0.0.0.0:%d" (int $s.doq.port)) "tls" $tls)) }}{{ end -}}
{{- if $s.doh.enabled }}{{ $_ := set $listeners "doh" (list (dict "address" (printf "0.0.0.0:%d" (int $s.doh.port)) "path" "/dns-query" "tls" $tls)) }}{{ end -}}
{{- if $s.doh3.enabled }}{{ $_ := set $listeners "doh3" (list (dict "address" (printf "0.0.0.0:%d" (int $s.doh3.port)) "path" "/dns-query" "tls" $tls)) }}{{ end -}}
{{- end -}}
{{- if .Values.blockPage.enabled -}}
{{- $bp := dict "http" (printf ":%d" (int .Values.blockPage.httpPort)) -}}
{{- if .Values.blockPage.https.enabled }}{{ $_ := set $bp "https" (printf ":%d" (int .Values.blockPage.https.port)) }}{{ end -}}
{{- $_ := set $listeners "blockPage" $bp -}}
{{- end -}}
{{- if eq $edgeMode "sidecar" -}}
{{- $_ := set $listeners "proxyProtocolFrom" (list "127.0.0.1/32") -}}
{{- else if $edgeMode -}}
{{- $_ := set $listeners "proxyProtocolFrom" .Values.edge.proxyProtocolFrom -}}
{{- $_ := set $listeners "proxyProtocolSecretFile" "/etc/kudurru/secrets/proxy/proxy-protocol-secret" -}}
{{- end -}}
{{- $base := dict
    "node" (dict "dataDir" "/var/lib/kudurru")
    "listeners" $listeners
    "store" (dict "driver" (ternary "sqlite" "kube" (eq $standalone "true"))) -}}
{{- if $standalone -}}
{{- if .Values.objects }}{{ $_ := set $base "objects" .Values.objects }}{{ end -}}
{{- else -}}
{{- /* Objects come from CRDs in mode=kubernetes; no objects.d directory. */ -}}
{{- $_ := set $base "objectsDir" "" -}}
{{- end -}}
{{- if .Values.externalDNS.webhook.enabled -}}
{{- $_ := set $base "externalDNS" (dict "webhook" (dict "listen" (printf ":%d" (int .Values.externalDNS.webhook.port)))) -}}
{{- end -}}
{{- with .Values.auth.oidc.providers -}}
{{- $_ := set $base "auth" (dict "identityProviders" .) -}}
{{- end -}}
{{- $cfg := mergeOverwrite $base (deepCopy (.Values.config.node | default dict)) -}}
{{- toYaml $cfg -}}
{{- end -}}

{{/* Fails the render on value combinations that cannot work. */}}
{{- define "kudurru.validate" -}}
{{- $v := .Values -}}
{{- if not (has $v.mode (list "kubernetes" "standalone")) -}}
{{- fail (printf "mode must be kubernetes or standalone, not %q" $v.mode) -}}
{{- end -}}
{{- if not (has $v.vip.provider (list "none" "kube-vip" "metallb" "keepalived")) -}}
{{- fail (printf "vip.provider must be none, kube-vip, metallb or keepalived, not %q" $v.vip.provider) -}}
{{- end -}}
{{- if not (has $v.edge.mode (list "deployment" "sidecar" "daemonset")) -}}
{{- fail (printf "edge.mode must be deployment, sidecar or daemonset, not %q" $v.edge.mode) -}}
{{- end -}}
{{- if eq $v.vip.provider "keepalived" -}}
{{- if not $v.edge.enabled }}{{ fail "vip.provider=keepalived needs edge.enabled=true (the VIP owner must run dnsdist on :53, §5.4)" }}{{ end -}}
{{- if eq $v.edge.mode "sidecar" }}{{ fail "vip.provider=keepalived cannot be combined with edge.mode=sidecar" }}{{ end -}}
{{- if not $v.vip.address }}{{ fail "vip.address (CIDR form, e.g. 10.0.0.53/24) is required with vip.provider=keepalived" }}{{ end -}}
{{- if not (contains "/" $v.vip.address) }}{{ fail "vip.address must be in CIDR form (10.0.0.53/24) with vip.provider=keepalived" }}{{ end -}}
{{- if not $v.vip.keepalived.interface }}{{ fail "vip.keepalived.interface is required with vip.provider=keepalived" }}{{ end -}}
{{- end -}}
{{- if and (has $v.vip.provider (list "kube-vip" "metallb")) (not $v.vip.address) -}}
{{- fail (printf "vip.address is required with vip.provider=%s" $v.vip.provider) -}}
{{- end -}}
{{- if include "kudurru.anyTLS" . -}}
{{- if and $v.edge.enabled (not $v.edge.tlsSecretName) }}{{ fail "service.dot/doh/doq/doh3 with edge.enabled need edge.tlsSecretName (dnsdist terminates TLS)" }}{{ end -}}
{{- if and (not $v.edge.enabled) (not $v.tls.secretName) }}{{ fail "service.dot/doh/doq/doh3 need tls.secretName (a kubernetes.io/tls Secret)" }}{{ end -}}
{{- end -}}
{{- if and (eq $v.mode "kubernetes") $v.hostNetwork.enabled (or $v.persistence.querylog.enabled $v.persistence.local.enabled) -}}
{{- fail "persistence.querylog/local need per-pod claims, which a hostNetwork DaemonSet cannot have; use extraVolumes (hostPath) instead" -}}
{{- end -}}
{{- if and $v.ingress.enabled (not $v.ingress.host) -}}
{{- fail "ingress.host is required when ingress.enabled" -}}
{{- end -}}
{{- range $i, $p := $v.auth.oidc.providers -}}
{{- if not $p.id }}{{ fail (printf "auth.oidc.providers[%d].id is required" $i) }}{{ end -}}
{{- if and $p.clientSecretRef (include "kudurru.standalone" $) }}{{ fail (printf "auth.oidc.providers[%d]: clientSecretRef needs mode=kubernetes; use clientSecretFile with extraVolumes" $i) }}{{ end -}}
{{- end -}}
{{- if and $v.edge.enabled (eq (int $v.edge.backendPort) 53) -}}
{{- fail "edge.backendPort must not be 53: the edge owns :53" -}}
{{- end -}}
{{- if and $v.blockPage.enabled (not (or $v.blockPage.ipv4 $v.blockPage.ipv6)) -}}
{{- fail "blockPage.enabled needs blockPage.ipv4 or blockPage.ipv6: the address of the block page Service that blocked names resolve to" -}}
{{- end -}}
{{- end -}}

{{/*
The settings file (/etc/kudurru/settings/settings.yaml): config.settings,
over the blockPage keys the blockPage values imply.
*/}}
{{- define "kudurru.settings" -}}
{{- $base := dict -}}
{{- if .Values.blockPage.enabled -}}
{{- $bp := dict "enabled" true -}}
{{- with .Values.blockPage.ipv4 }}{{ $_ := set $bp "ipv4" . }}{{ end -}}
{{- with .Values.blockPage.ipv6 }}{{ $_ := set $bp "ipv6" . }}{{ end -}}
{{- $_ := set $base "blockPage" $bp -}}
{{- end -}}
{{- toYaml (mergeOverwrite $base (deepCopy (.Values.config.settings | default dict))) -}}
{{- end -}}

{{/* A map rendered as a label selector string: k1=v1,k2=v2 (sorted keys). */}}
{{- define "kudurru.labelSelector" -}}
{{- $parts := list -}}
{{- range $k, $v := . -}}
{{- $parts = append $parts (printf "%s=%s" $k $v) -}}
{{- end -}}
{{- join "," $parts -}}
{{- end -}}

{{/*
"true" when the kudurru pod binds a port below 1024 in its own network
namespace (plain DNS, a sidecar dnsdist on :53, or DoT/DoH/DoQ listeners).
hostNetwork pods cannot set namespaced sysctls (docs/deploy/helm.md).
*/}}
{{- define "kudurru.lowPortSysctl" -}}
{{- if not .Values.hostNetwork.enabled -}}
{{- $edgeMode := include "kudurru.edge.mode" . -}}
{{- $s := .Values.service -}}
{{- if or (lt (int (include "kudurru.dnsPort" .)) 1024) (eq $edgeMode "sidecar") (and (not .Values.edge.enabled) (include "kudurru.anyTLS" .)) -}}
true
{{- end -}}
{{- end -}}
{{- end -}}
