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
{{/*
hostNetwork pods run the <tag>-hostnetwork variant: the same binary with
cap_net_bind_service=+ep, because the ip_unprivileged_port_start sysctl is
not available on the host network (images/hostnetwork/Dockerfile). A digest
is used as given.
*/}}
{{- define "kudurru.image" -}}
{{- if .Values.image.digest -}}
{{ .Values.image.repository }}@{{ .Values.image.digest }}
{{- else -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}{{ if .Values.hostNetwork.enabled }}{{ .Values.hostNetwork.imageTagSuffix }}{{ end }}
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

{{/*
"true" when the dnsdist container starts as root and dnsdist drops to 953:
eBPF DynBlocks (BPF maps need CAP_BPF/CAP_SYS_ADMIN, which Kubernetes only
makes effective for root), or dnsdist on the host network (the edge
DaemonSet, or the sidecar of a hostNetwork kudurru pod), where the
ip_unprivileged_port_start sysctl that lets 953 bind :53 is not available.
Takes (dict "root" $ "sidecar" bool).
*/}}
{{- define "kudurru.edge.rootStart" -}}
{{- $v := .root.Values -}}
{{- $hostNet := ternary $v.hostNetwork.enabled (eq (include "kudurru.edge.mode" .root) "daemonset") (eq (toString .sidecar) "true") -}}
{{- if or $v.edge.ebpf.enabled $hostNet }}true{{ end -}}
{{- end -}}

{{/* "true" when a separate edge workload (not the sidecar) fronts kudurru. */}}
{{- define "kudurru.edge.separate" -}}
{{- $m := include "kudurru.edge.mode" . -}}
{{- if or (eq $m "deployment") (eq $m "daemonset") }}true{{ end -}}
{{- end -}}

{{/* Legacy data claim selection is independent of separate component mounts. */}}
{{- define "kudurru.dataPersistence" -}}
{{- if include "kudurru.standalone" . -}}
{{- if .Values.persistence.standalone.enabled }}{{ toYaml .Values.persistence.standalone }}{{ end -}}
{{- else if .Values.persistence.querylog.enabled -}}
{{ toYaml .Values.persistence.querylog }}
{{- else if .Values.persistence.local.enabled -}}
{{ toYaml .Values.persistence.local }}
{{- end -}}
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

{{- define "kudurru.licenseSecret" -}}
{{- default (printf "%s-license" (include "kudurru.fullname" .)) .Values.license.existingSecret -}}
{{- end -}}

{{/* The license key env of every kudurru container (serve, edge, keepalived).
     optional: pods start without the Secret and run with the community license. */}}
{{- define "kudurru.licenseEnv" -}}
- name: KUDURRU_LICENSE_KEY
  valueFrom:
    secretKeyRef:
      name: {{ include "kudurru.licenseSecret" . }}
      key: {{ .Values.license.secretKey }}
      optional: true
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
{{- $node := dict "dataDir" "/var/lib/kudurru" -}}
{{- if .Values.persistence.querylog.separate }}{{ $_ := set $node "querylogDir" "/var/lib/kudurru-querylog" }}{{ end -}}
{{- if .Values.persistence.local.separate }}{{ $_ := set $node "localDir" "/var/lib/kudurru-local" }}{{ end -}}
{{- $base := dict
    "node" $node
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
{{- if .Values.dhcp.enabled -}}
{{- $_ := set $base "dhcp" (dict "enabled" true "serverIP" .Values.dhcp.serverAddress "interface" .Values.dhcp.interface) -}}
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
{{- if $v.dhcp.enabled -}}
{{- if ne $v.mode "kubernetes" -}}
{{- fail "dhcp.enabled serves DHCPScope objects and needs mode kubernetes; in mode standalone, configure DHCP with config.node.dhcp" -}}
{{- end -}}
{{- if not $v.dhcp.serverAddress -}}
{{- fail "dhcp.enabled needs dhcp.serverAddress: the LoadBalancer address DHCP relays send to" -}}
{{- end -}}
{{- if and (has $v.dhcp.service.type (list "LoadBalancer" "NodePort")) (ne $v.dhcp.service.externalTrafficPolicy "Local") -}}
{{- fail "dhcp.service.externalTrafficPolicy must be Local: trusted DHCP relays are matched by their source address" -}}
{{- end -}}
{{- if $v.hostNetwork.enabled -}}
{{- fail "dhcp.enabled reaches clients through a relay and its LoadBalancer Service; it does not support hostNetwork.enabled" -}}
{{- end -}}
{{- end -}}
{{- if not (has $v.service.api.sessionAffinity (list "None" "ClientIP")) -}}
{{- fail "service.api.sessionAffinity must be None or ClientIP" -}}
{{- end -}}
{{- if and (eq $v.service.api.sessionAffinity "ClientIP") $v.service.api.sessionAffinityConfig -}}
{{- $timeout := ($v.service.api.sessionAffinityConfig.clientIP | default dict).timeoutSeconds -}}
{{- if and (not (kindIs "invalid" $timeout)) (or (not (regexMatch "^[0-9]+$" (toString $timeout))) (lt (int $timeout) 1) (gt (int $timeout) 86400)) -}}
{{- fail "service.api.sessionAffinityConfig.clientIP.timeoutSeconds must be an integer in 1-86400" -}}
{{- end -}}
{{- end -}}
{{- if not (has $v.vip.provider (list "none" "kube-vip" "metallb" "keepalived")) -}}
{{- fail (printf "vip.provider must be none, kube-vip, metallb or keepalived, not %q" $v.vip.provider) -}}
{{- end -}}
{{- if not (has $v.edge.mode (list "deployment" "sidecar" "daemonset")) -}}
{{- fail (printf "edge.mode must be deployment, sidecar or daemonset, not %q" $v.edge.mode) -}}
{{- end -}}
{{- $hmacRefs := $v.vip.keepalived.hmacKeySecrets -}}
{{- if not (kindIs "slice" $hmacRefs) }}{{ fail "vip.keepalived.hmacKeySecrets must be a list of Secret references" }}{{ end -}}
{{- if gt (len $hmacRefs) 255 }}{{ fail "vip.keepalived.hmacKeySecrets allows at most 255 references" }}{{ end -}}
{{- if and $hmacRefs (ne $v.vip.provider "keepalived") }}{{ fail "vip.keepalived.hmacKeySecrets requires vip.provider=keepalived" }}{{ end -}}
{{- if and $hmacRefs (or $v.vip.keepalived.existingSecret $v.vip.keepalived.hmacSecret) }}{{ fail "per-VRID HMAC references cannot be combined with shared existingSecret or hmacSecret" }}{{ end -}}
{{- if eq $v.vip.provider "keepalived" -}}
{{- if gt (len $v.vip.keepalived.vips) 255 }}{{ fail "keepalived allows at most 255 VIP VRIDs" }}{{ end -}}
{{- if not $v.edge.enabled }}{{ fail "vip.provider=keepalived needs edge.enabled=true (the VIP owner must run dnsdist on :53, §5.4)" }}{{ end -}}
{{- if eq $v.edge.mode "sidecar" }}{{ fail "vip.provider=keepalived cannot be combined with edge.mode=sidecar" }}{{ end -}}
{{- $names := dict -}}
{{- $vrids := dict -}}
{{- $addresses := dict -}}
{{- range (include "kudurru.keepalived.vips" . | fromYamlArray) -}}
{{- if not (regexMatch "^[a-z0-9][a-z0-9_-]{0,30}$" .name) }}{{ fail "keepalived VIP name must be 1-31 lowercase letters, digits, underscores or hyphens" }}{{ end -}}
{{- if not .address }}{{ fail "vip.address or vip.keepalived.vips[].address (CIDR form) is required with vip.provider=keepalived" }}{{ end -}}
{{- if not (contains "/" .address) }}{{ fail "keepalived VIP address must be in CIDR form (10.0.0.53/24)" }}{{ end -}}
{{- if not .interface }}{{ fail "vip.keepalived.interface or a VIP interface is required with vip.provider=keepalived" }}{{ end -}}
{{- if not (regexMatch "^[A-Za-z0-9_.:-]+$" .interface) }}{{ fail "keepalived VIP interface is invalid" }}{{ end -}}
{{- if or (not (regexMatch "^[0-9]+$" (toString .vrid))) (lt (atoi (toString .vrid)) 1) (gt (atoi (toString .vrid)) 255) }}{{ fail "keepalived VIP vrid must be an integer in 1-255" }}{{ end -}}
{{- if or (not (regexMatch "^[0-9]+$" (toString .preemptDelay))) (gt (int .preemptDelay) 1000) }}{{ fail "keepalived VIP preemptDelay must be an integer in 0-1000" }}{{ end -}}
{{- if hasKey $names .name }}{{ fail "keepalived VIP names must be unique" }}{{ end -}}
{{- if hasKey $vrids (toString (atoi (toString .vrid))) }}{{ fail "keepalived VIP VRIDs must be unique" }}{{ end -}}
{{/* Compare host text without the prefix length. Alternate IPv6 spellings
are checked by the supervisor's parsed-address validation. */}}
{{- $address := first (splitList "/" .address) -}}
{{- if hasKey $addresses $address }}{{ fail "keepalived VIP addresses must be unique" }}{{ end -}}
{{- $_ := set $names .name true -}}
{{- $_ := set $vrids (toString (atoi (toString .vrid))) true -}}
{{- $_ := set $addresses $address true -}}
{{- end -}}
{{- $mapped := dict -}}
{{- range $hmacRefs -}}
{{- if not (kindIs "map" .) }}{{ fail "hmacKeySecrets entries must be Secret references" }}{{ end -}}
{{- range $field := keys . -}}
{{- if not (has $field (list "vrid" "secretName" "key")) }}{{ fail "hmacKeySecrets entries accept only vrid, secretName and key; no key values" }}{{ end -}}
{{- end -}}
{{- if or (not (regexMatch "^[1-9][0-9]{0,2}$" (toString .vrid))) (gt (atoi (toString .vrid)) 255) }}{{ fail "HMAC reference vrid must be an integer in 1-255" }}{{ end -}}
{{- $id := toString (atoi (toString .vrid)) -}}
{{- if hasKey $mapped $id }}{{ fail "HMAC reference VRIDs must be unique" }}{{ end -}}
{{- if not (hasKey $vrids $id) }}{{ fail "HMAC reference VRID has no configured VIP" }}{{ end -}}
{{- if not (kindIs "string" .secretName) }}{{ fail "HMAC reference secretName is required" }}{{ end -}}
{{- if or (gt (len .secretName) 253) (not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$" .secretName)) }}{{ fail "HMAC reference secretName must be a valid Kubernetes Secret name" }}{{ end -}}
{{- range (splitList "." .secretName) -}}
{{- if gt (len .) 63 }}{{ fail "HMAC reference Secret name labels must be at most 63 characters" }}{{ end -}}
{{- end -}}
{{- if not (kindIs "string" .key) }}{{ fail "HMAC reference key is required" }}{{ end -}}
{{- if or (gt (len .key) 253) (not (regexMatch "^[A-Za-z0-9._-]+$" .key)) }}{{ fail "HMAC reference key must be a valid Kubernetes Secret key" }}{{ end -}}
{{- $_ := set $mapped $id true -}}
{{- end -}}
{{- if and $hmacRefs (ne (len $mapped) (len $vrids)) }}{{ fail "HMAC references must cover every configured VIP VRID" }}{{ end -}}
{{- if and $v.vip.keepalived.vrrpInterface (not (regexMatch "^[A-Za-z0-9_.:-]+$" $v.vip.keepalived.vrrpInterface)) }}{{ fail "vip.keepalived.vrrpInterface is invalid" }}{{ end -}}
{{- end -}}
{{- if and (has $v.vip.provider (list "kube-vip" "metallb")) (not $v.vip.address) -}}
{{- fail (printf "vip.address is required with vip.provider=%s" $v.vip.provider) -}}
{{- end -}}
{{- if and (has $v.vip.provider (list "kube-vip" "metallb")) (ne $v.service.dns.type "LoadBalancer") -}}
{{- fail "Service-based vip.provider requires service.dns.type=LoadBalancer" -}}
{{- end -}}
{{- if include "kudurru.anyTLS" . -}}
{{- if and $v.edge.enabled (not $v.edge.tlsSecretName) }}{{ fail "service.dot/doh/doq/doh3 with edge.enabled need edge.tlsSecretName (dnsdist terminates TLS)" }}{{ end -}}
{{- if and (not $v.edge.enabled) (not $v.tls.secretName) }}{{ fail "service.dot/doh/doq/doh3 need tls.secretName (a kubernetes.io/tls Secret)" }}{{ end -}}
{{- end -}}
{{- if and $v.hostNetwork.enabled $v.hostNetwork.imageTagSuffix (not (has "NET_BIND_SERVICE" ((($v.securityContext | default dict).capabilities | default dict).add | default list))) -}}
{{- fail "hostNetwork.enabled runs the -hostnetwork image, whose binary carries cap_net_bind_service=+ep: securityContext.capabilities.add must keep NET_BIND_SERVICE, or the kernel refuses to start it" -}}
{{- end -}}
{{- if and (eq $v.mode "kubernetes") $v.hostNetwork.enabled (or $v.persistence.querylog.enabled $v.persistence.local.enabled) -}}
{{- fail "persistence.querylog/local need per-pod claims, which a hostNetwork DaemonSet cannot have; use extraVolumes (hostPath) instead" -}}
{{- end -}}
{{- range $name := list "querylog" "local" -}}
{{- $p := index $v.persistence $name -}}
{{- if $p.separate -}}
{{- $path := printf "/var/lib/kudurru-%s" $name -}}
{{- $key := printf "%sDir" $name -}}
{{- $node := ($v.config.node.node | default dict) -}}
{{- if and (hasKey $node $key) (ne (index $node $key) $path) -}}
{{- fail (printf "persistence.%s.separate requires config.node.node.%s=%s" $name $key $path) -}}
{{- end -}}
{{- if $p.enabled -}}
{{- if or (ne (len $p.accessModes) 1) (not (has (first $p.accessModes) (list "ReadWriteOnce" "ReadWriteOncePod"))) -}}
{{- fail (printf "persistence.%s.separate requires ReadWriteOnce or ReadWriteOncePod; SQLite needs node-local compatible storage, never RWX/NFS" $name) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if and $v.ingress.enabled (not $v.ingress.host) -}}
{{- fail "ingress.host is required when ingress.enabled" -}}
{{- end -}}
{{- if not (has $v.ingress.kind (list "Ingress" "IngressRoute")) -}}
{{- fail "ingress.kind must be Ingress or IngressRoute" -}}
{{- end -}}
{{- if and $v.ingress.enabled (eq $v.ingress.kind "IngressRoute") -}}
{{- if $v.ingress.tls }}{{ fail "IngressRoute uses ingress.traefik.tls, not ingress.tls" }}{{ end -}}
{{- if not $v.ingress.traefik.entryPoints }}{{ fail "ingress.traefik.entryPoints must list at least one entry point" }}{{ end -}}
{{- if regexMatch "[`[:space:]]" $v.ingress.host }}{{ fail "ingress.host must be a hostname without spaces or backticks" }}{{ end -}}
{{- end -}}
{{- if and $v.ingress.enabled $v.ingress.traefik.sticky.enabled (or (eq $v.ingress.kind "IngressRoute") (eq $v.ingress.className "traefik")) -}}
{{- if not (has $v.ingress.traefik.sticky.cookie.sameSite (list "none" "lax" "strict" "None" "Lax" "Strict")) }}{{ fail "ingress.traefik.sticky.cookie.sameSite must be none, lax or strict" }}{{ end -}}
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

{{/* Traefik's standard Ingress provider reads cookie affinity from Service
annotations. IngressRoute carries its own sticky.cookie settings instead.
Explicit service annotations override the generated defaults. */}}
{{- define "kudurru.api.annotations" -}}
{{- $ann := dict -}}
{{- $i := .Values.ingress -}}
{{- if and $i.enabled (eq $i.kind "Ingress") (eq $i.className "traefik") $i.traefik.sticky.enabled -}}
{{- $_ := set $ann "traefik.ingress.kubernetes.io/service.sticky.cookie" "true" -}}
{{- $_ := set $ann "traefik.ingress.kubernetes.io/service.nativelb" "false" -}}
{{- range $k, $v := $i.traefik.sticky.cookie -}}
{{- $_ := set $ann (printf "traefik.ingress.kubernetes.io/service.sticky.cookie.%s" (lower $k)) (toString $v) -}}
{{- end -}}
{{- end -}}
{{- toYaml (mergeOverwrite $ann (deepCopy (.Values.service.api.annotations | default dict))) -}}
{{- end -}}

{{/* Normalize the legacy single VIP and the multiple-VIP list. */}}
{{- define "kudurru.keepalived.vips" -}}
{{- $k := .Values.vip.keepalived -}}
{{- $vips := list -}}
{{- if $k.vips -}}
{{- range $k.vips -}}
{{- $vips = append $vips (dict "name" (.name | default "") "address" (.address | default "") "interface" (.interface | default $k.interface) "vrid" (.vrid | default 0) "preemptDelay" (.preemptDelay | default 0)) -}}
{{- end -}}
{{- else -}}
{{- $vips = list (dict "name" "dns" "address" .Values.vip.address "interface" $k.interface "vrid" $k.vrid "preemptDelay" 0) -}}
{{- end -}}
{{- toYaml $vips -}}
{{- end -}}

{{/*
"true" when the kudurru pod binds a port below 1024 in its own network
namespace (plain DNS, a sidecar dnsdist on :53, DoT/DoH/DoQ listeners, or
DHCP on UDP 67).
hostNetwork pods cannot set namespaced sysctls (docs/deploy/helm.md).
*/}}
{{- define "kudurru.lowPortSysctl" -}}
{{- if not .Values.hostNetwork.enabled -}}
{{- $edgeMode := include "kudurru.edge.mode" . -}}
{{- $s := .Values.service -}}
{{- if or (lt (int (include "kudurru.dnsPort" .)) 1024) (eq $edgeMode "sidecar") (and (not .Values.edge.enabled) (include "kudurru.anyTLS" .)) .Values.dhcp.enabled -}}
true
{{- end -}}
{{- end -}}
{{- end -}}

{{/* Fixed filenames only; no Secret metadata enters shell commands. */}}
{{- define "kudurru.keepalived.hmacFiles" -}}
{{- $files := list "vrrp-hmac" -}}
{{- if .Values.vip.keepalived.hmacKeySecrets -}}
{{- $files = list -}}
{{- range .Values.vip.keepalived.hmacKeySecrets -}}
{{- $files = append $files (printf "vrrp_hmac_%d" (atoi (toString .vrid))) -}}
{{- end -}}
{{- end -}}
{{- toYaml $files -}}
{{- end -}}

{{/* DNS Service provider contract: ee/edge/vip.ServiceProvider, with render
     parity tests in deploy. Explicit provider values override matching user
     annotations; unrelated annotations are preserved. Preserve the historical
     split-Service sharing key for provider=none (externally managed MetalLB). */}}
{{- define "kudurru.dns.annotations" -}}
{{- $svc := .Values.service.dns -}}
{{- $ann := deepCopy ($svc.annotations | default dict) -}}
{{- if eq .Values.vip.provider "kube-vip" -}}
{{- $_ := set $ann "kube-vip.io/loadbalancerIPs" .Values.vip.address -}}
{{- else if eq .Values.vip.provider "metallb" -}}
{{- $_ := set $ann "metallb.io/loadBalancerIPs" .Values.vip.address -}}
{{- if .Values.vip.metallbPool }}{{ $_ := set $ann "metallb.io/address-pool" .Values.vip.metallbPool }}{{ end -}}
{{- end -}}
{{- if and $svc.splitProtocols (has .Values.vip.provider (list "none" "metallb")) -}}
{{- $_ := set $ann "metallb.io/allow-shared-ip" (include "kudurru.fullname" .) -}}
{{- end -}}
{{- toYaml $ann -}}
{{- end -}}
