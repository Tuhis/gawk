{{- define "gawk-whip-gateway.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end }}

{{- define "gawk-whip-gateway.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "gawk-whip-gateway.selectorLabels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
One `foo` / `fooRef` secret env var, the house dual form: Ref wins, and the
literal never renders beside a Ref.
*/}}
{{- define "gawk-whip-gateway.secretEnv" -}}
- name: {{ .name }}
  {{- if and .ref .ref.name }}
  valueFrom:
    secretKeyRef:
      name: {{ .ref.name | quote }}
      key: {{ .ref.key | quote }}
  {{- else }}
  value: {{ .value | default "" | quote }}
  {{- end }}
{{- end }}

{{/* The headless Service the StatefulSet's pods resolve each other by (D10). */}}
{{- define "gawk-whip-gateway.peersName" -}}
{{- printf "%s-peers" (include "gawk-whip-gateway.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end }}
