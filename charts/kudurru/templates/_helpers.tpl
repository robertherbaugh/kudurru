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

{{- define "kudurru.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "kudurru.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "kudurru.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kudurru.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: dns
{{- end -}}

{{- define "kudurru.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "kudurru.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "kudurru.image" -}}
{{- if .Values.image.digest -}}
{{ .Values.image.repository }}@{{ .Values.image.digest }}
{{- else -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end -}}
{{- end -}}

{{- define "kudurru.nodeConfig" -}}
{{- $cfg := deepCopy .Values.config.node -}}
{{- $listeners := dict "dns" (list (dict "address" (printf "0.0.0.0:%d" (int .Values.listen.port)) "protocols" (list "udp" "tcp"))) "api" (dict "address" "0.0.0.0:8080") "ops" (dict "address" "0.0.0.0:8081") "metrics" (dict "address" "0.0.0.0:9153") -}}
{{- $_ := set $cfg "listeners" (merge (default dict (get $cfg "listeners")) $listeners) -}}
{{- $_ := set $cfg "node" (merge (default dict (get $cfg "node")) (dict "dataDir" "/var/lib/kudurru")) -}}
{{- merge $cfg .Values.config.settings | toYaml -}}
{{- end -}}
