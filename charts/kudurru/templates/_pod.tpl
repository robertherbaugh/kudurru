{{/*
The kudurru pod template shared by the Deployment, DaemonSet and StatefulSet
(docs/architecture.md §5.2). Probes on the ops listener :8081; secrets as
files through KUDURRU_*_FILE; the node ConfigMap checksum rolls the pods
(D25).
*/}}
{{- define "kudurru.podTemplate" -}}
{{- $kind := include "kudurru.workloadKind" . -}}
{{- $edgeMode := include "kudurru.edge.mode" . -}}
{{- $dnsPort := int (include "kudurru.dnsPort" .) -}}
{{- $s := .Values.service -}}
metadata:
  labels:
    {{- include "kudurru.labels" . | nindent 4 }}
    {{- with .Values.podLabels }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
  annotations:
    # Only the node file rolls pods (D25).
    checksum/node-config: {{ include "kudurru.nodeConfig" . | sha256sum }}
    kubectl.kubernetes.io/default-container: kudurru
    {{- with .Values.podAnnotations }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
spec:
  serviceAccountName: {{ include "kudurru.serviceAccountName" . }}
  automountServiceAccountToken: {{ not (include "kudurru.standalone" .) }}
  enableServiceLinks: false
  {{- if eq $kind "Deployment" }}
  # <pod>.<headless> resolves for the leader lookup of serve.go.
  subdomain: {{ include "kudurru.headlessName" . }}
  {{- end }}
  {{- with .Values.imagePullSecrets }}
  imagePullSecrets:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- if .Values.priorityClassName }}
  priorityClassName: {{ .Values.priorityClassName }}
  {{- end }}
  {{- if .Values.hostNetwork.enabled }}
  hostNetwork: true
  dnsPolicy: ClusterFirstWithHostNet
  {{- else }}
  dnsPolicy: {{ .Values.dnsPolicy }}
  {{- end }}
  terminationGracePeriodSeconds: {{ .Values.terminationGracePeriodSeconds }}
  securityContext:
    {{- toYaml .Values.podSecurityContext | nindent 4 }}
  containers:
    - name: kudurru
      image: {{ include "kudurru.image" . }}
      imagePullPolicy: {{ .Values.image.pullPolicy }}
      args:
        - serve
        - --config=/etc/kudurru/node/kudurru.yaml
      env:
        - name: KUDURRU_NODE__NAME
          valueFrom:
            fieldRef:
              fieldPath: metadata.name
        - name: KUDURRU_HEADLESS_SERVICE
          value: {{ include "kudurru.headlessFQDN" . | quote }}
        - name: KUDURRU_ENCRYPTION_KEY_FILE
          value: /etc/kudurru/secrets/encryption/key
        {{- if .Values.auth.encryptionKeyPreviousSecret }}
        - name: KUDURRU_ENCRYPTION_KEY_PREVIOUS_FILE
          value: /etc/kudurru/secrets/encryption-previous/key
        {{- end }}
        - name: KUDURRU_ADMIN_PASSWORD_FILE
          value: /etc/kudurru/secrets/admin/password
        {{- if .Values.auth.bootstrapToken.enabled }}
        - name: KUDURRU_BOOTSTRAP_TOKEN_FILE
          value: /etc/kudurru/secrets/bootstrap-token/token
        {{- end }}
        {{- if (.Values.resources.limits | default dict).memory }}
        - name: GOMEMLIMIT
          valueFrom:
            resourceFieldRef:
              resource: limits.memory
              divisor: "1"
        {{- end }}
        {{- with .Values.extraEnv }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
      ports:
        {{- if ne $edgeMode "sidecar" }}
        - name: dns-udp
          containerPort: {{ $dnsPort }}
          protocol: UDP
        - name: dns-tcp
          containerPort: {{ $dnsPort }}
          protocol: TCP
        {{- end }}
        {{- if not .Values.edge.enabled }}
        {{- if $s.dot.enabled }}
        - name: dot
          containerPort: {{ $s.dot.port }}
          protocol: TCP
        {{- end }}
        {{- if $s.doq.enabled }}
        - name: doq
          containerPort: {{ $s.doq.port }}
          protocol: UDP
        {{- end }}
        {{- if $s.doh.enabled }}
        - name: doh
          containerPort: {{ $s.doh.port }}
          protocol: TCP
        {{- end }}
        {{- if $s.doh3.enabled }}
        - name: doh3
          containerPort: {{ $s.doh3.port }}
          protocol: UDP
        {{- end }}
        {{- end }}
        - name: api
          containerPort: 8080
          protocol: TCP
        - name: ops
          containerPort: 8081
          protocol: TCP
        - name: internal
          containerPort: 8082
          protocol: TCP
        - name: metrics
          containerPort: 9153
          protocol: TCP
        {{- if .Values.externalDNS.webhook.enabled }}
        - name: webhook
          containerPort: {{ .Values.externalDNS.webhook.port }}
          protocol: TCP
        {{- end }}
      startupProbe:
        httpGet:
          path: /healthz/startup
          port: ops
        periodSeconds: {{ .Values.probes.startup.periodSeconds }}
        failureThreshold: {{ .Values.probes.startup.failureThreshold }}
      readinessProbe:
        httpGet:
          path: /healthz/ready
          port: ops
        periodSeconds: {{ .Values.probes.readiness.periodSeconds }}
        failureThreshold: {{ .Values.probes.readiness.failureThreshold }}
      livenessProbe:
        httpGet:
          path: /healthz/live
          port: ops
        periodSeconds: {{ .Values.probes.liveness.periodSeconds }}
        failureThreshold: {{ .Values.probes.liveness.failureThreshold }}
      {{- if gt (int .Values.lameduckSeconds) 0 }}
      lifecycle:
        preStop:
          sleep:
            seconds: {{ .Values.lameduckSeconds }}
      {{- end }}
      securityContext:
        {{- toYaml .Values.securityContext | nindent 8 }}
      resources:
        {{- toYaml .Values.resources | nindent 8 }}
      volumeMounts:
        - name: node-config
          mountPath: /etc/kudurru/node
          readOnly: true
        - name: settings
          mountPath: /etc/kudurru/settings
          readOnly: true
        - name: encryption-key
          mountPath: /etc/kudurru/secrets/encryption
          readOnly: true
        {{- if .Values.auth.encryptionKeyPreviousSecret }}
        - name: encryption-key-previous
          mountPath: /etc/kudurru/secrets/encryption-previous
          readOnly: true
        {{- end }}
        - name: admin
          mountPath: /etc/kudurru/secrets/admin
          readOnly: true
        {{- if .Values.auth.bootstrapToken.enabled }}
        - name: bootstrap-token
          mountPath: /etc/kudurru/secrets/bootstrap-token
          readOnly: true
        {{- end }}
        {{- if and $edgeMode (ne $edgeMode "sidecar") }}
        - name: proxy-protocol
          mountPath: /etc/kudurru/secrets/proxy
          readOnly: true
        {{- end }}
        {{- if and (not .Values.edge.enabled) (include "kudurru.anyTLS" .) }}
        - name: tls
          mountPath: /etc/kudurru/tls
          readOnly: true
        {{- end }}
        - name: data
          mountPath: /var/lib/kudurru
        - name: run
          mountPath: /run/kudurru
        - name: tmp
          mountPath: /tmp
        {{- with .Values.extraVolumeMounts }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
    {{- if eq $edgeMode "sidecar" }}
    {{- include "kudurru.edge.container" (dict "root" . "sidecar" true) | nindent 4 }}
    {{- end }}
  volumes:
    - name: node-config
      configMap:
        name: {{ include "kudurru.fullname" . }}-node
    - name: settings
      configMap:
        name: {{ include "kudurru.fullname" . }}-settings
    - name: encryption-key
      secret:
        secretName: {{ include "kudurru.encryptionKeySecret" . }}
        defaultMode: 0440
    {{- if .Values.auth.encryptionKeyPreviousSecret }}
    - name: encryption-key-previous
      secret:
        secretName: {{ .Values.auth.encryptionKeyPreviousSecret }}
        defaultMode: 0440
    {{- end }}
    - name: admin
      secret:
        secretName: {{ include "kudurru.adminSecret" . }}
        defaultMode: 0440
    {{- if .Values.auth.bootstrapToken.enabled }}
    - name: bootstrap-token
      secret:
        secretName: {{ include "kudurru.bootstrapTokenSecret" . }}
        defaultMode: 0440
    {{- end }}
    {{- if and $edgeMode (ne $edgeMode "sidecar") }}
    - name: proxy-protocol
      secret:
        secretName: {{ include "kudurru.proxySecret" . }}
        defaultMode: 0440
    {{- end }}
    {{- if and (not .Values.edge.enabled) (include "kudurru.anyTLS" .) }}
    - name: tls
      secret:
        secretName: {{ .Values.tls.secretName }}
        defaultMode: 0440
    {{- end }}
    {{- if and .Values.edge.enabled .Values.edge.tlsSecretName (eq $edgeMode "sidecar") }}
    - name: edge-tls
      secret:
        secretName: {{ .Values.edge.tlsSecretName }}
        defaultMode: 0440
    {{- end }}
    {{- if eq $edgeMode "sidecar" }}
    - name: dnsdist-config
      emptyDir: {}
    {{- end }}
    {{- if ne $kind "StatefulSet" }}
    - name: data
      emptyDir: {}
    {{- else if and (include "kudurru.standalone" .) (not .Values.persistence.standalone.enabled) }}
    - name: data
      emptyDir: {}
    {{- end }}
    - name: run
      emptyDir:
        medium: Memory
        sizeLimit: 16Mi
    - name: tmp
      emptyDir:
        sizeLimit: 256Mi
    {{- with .Values.extraVolumes }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
  {{- with .Values.nodeSelector }}
  nodeSelector:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with .Values.affinity }}
  affinity:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with .Values.tolerations }}
  tolerations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- if and (ne $kind "DaemonSet") .Values.topologySpreadConstraints }}
  topologySpreadConstraints:
    {{- range .Values.topologySpreadConstraints }}
    - {{ toYaml . | nindent 6 | trim }}
      labelSelector:
        matchLabels:
          {{- include "kudurru.selectorLabels" $ | nindent 10 }}
    {{- end }}
  {{- end }}
{{- end -}}

{{/*
The dnsdist supervisor container (`kudurru edge run`, §3.6). Used by the
edge Deployment/DaemonSet and, with sidecar=true, inside the kudurru pod.
*/}}
{{- define "kudurru.edge.container" -}}
{{- $root := .root -}}
{{- $e := $root.Values.edge -}}
{{- $s := $root.Values.service -}}
- name: dnsdist
  image: {{ include "kudurru.edge.image" $root }}
  imagePullPolicy: {{ $e.image.pullPolicy }}
  command: ["/usr/local/bin/kudurru", "edge", "run"]
  args:
    {{- if .sidecar }}
    - --backends=127.0.0.1:{{ $e.backendPort }}
    {{- else }}
    - --backends-from=endpointslice:{{ $root.Release.Namespace }}/{{ include "kudurru.headlessName" $root }}
    - --backend-port={{ $e.backendPort }}
    - --proxy-protocol-secret-file=/etc/kudurru/secrets/proxy/proxy-protocol-secret
    {{- end }}
    - --pool-resolver=chashed
    - --pool-authoritative=leastOutstanding
    - --api=127.0.0.1:8083
    - --console=127.0.0.1:5199
    - --api-key-out=/run/kudurru/dnsdist-api.key
    - --readiness-address=0.0.0.0:8084
    {{- if $e.tlsSecretName }}
    - --tls-dir=/certs
    {{- end }}
    {{- if $s.dot.enabled }}
    - --dot=0.0.0.0:{{ $s.dot.port }}
    {{- end }}
    {{- if $s.doh.enabled }}
    - --doh=0.0.0.0:{{ $s.doh.port }}
    {{- end }}
    {{- if $s.doq.enabled }}
    - --doq=0.0.0.0:{{ $s.doq.port }}
    {{- end }}
    {{- if $s.doh3.enabled }}
    - --doh3=0.0.0.0:{{ $s.doh3.port }}
    {{- end }}
    {{- if $e.authoritativeCache }}
    - --authoritative-cache
    {{- end }}
    {{- if $e.dynblocks.enabled }}
    - --dynblocks
    {{- end }}
    {{- if $e.ebpf.enabled }}
    - --ebpf
    {{- end }}
    {{- with $e.extraArgs }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
  env:
    - name: KUDURRU_NODE__NAME
      valueFrom:
        fieldRef:
          fieldPath: metadata.name
  ports:
    - name: edge-dns-udp
      containerPort: 53
      protocol: UDP
    - name: edge-dns-tcp
      containerPort: 53
      protocol: TCP
    {{- if $s.dot.enabled }}
    - name: edge-dot
      containerPort: {{ $s.dot.port }}
      protocol: TCP
    {{- end }}
    {{- if $s.doq.enabled }}
    - name: edge-doq
      containerPort: {{ $s.doq.port }}
      protocol: UDP
    {{- end }}
    {{- if $s.doh.enabled }}
    - name: edge-doh
      containerPort: {{ $s.doh.port }}
      protocol: TCP
    {{- end }}
    {{- if $s.doh3.enabled }}
    - name: edge-doh3
      containerPort: {{ $s.doh3.port }}
      protocol: UDP
    {{- end }}
    - name: edge-ready
      containerPort: 8084
      protocol: TCP
  readinessProbe:
    httpGet:
      path: /healthz/ready
      port: edge-ready
    periodSeconds: 2
    failureThreshold: 2
  livenessProbe:
    httpGet:
      path: /healthz/live
      port: edge-ready
    periodSeconds: 10
    failureThreshold: 3
  securityContext:
    {{- $sc := deepCopy $e.securityContext -}}
    {{- if $e.ebpf.enabled }}
    {{- $_ := set $sc "capabilities" (dict "drop" (list "ALL") "add" (list "NET_BIND_SERVICE" "BPF" "SYS_ADMIN")) }}
    {{- end }}
    {{- toYaml $sc | nindent 4 }}
  resources:
    {{- toYaml $e.resources | nindent 4 }}
  volumeMounts:
    - name: dnsdist-config
      mountPath: /etc/dnsdist
    - name: run
      mountPath: /run/kudurru
    {{- if not .sidecar }}
    - name: proxy-protocol
      mountPath: /etc/kudurru/secrets/proxy
      readOnly: true
    - name: tmp
      mountPath: /tmp
    {{- end }}
    {{- if $e.tlsSecretName }}
    - name: edge-tls
      mountPath: /certs
      readOnly: true
    {{- end }}
{{- end -}}
