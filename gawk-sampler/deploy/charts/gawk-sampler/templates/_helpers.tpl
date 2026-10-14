{{- define "gawk-sampler.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end }}

{{- define "gawk-sampler.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "gawk-sampler.selectorLabels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
The image reference, in ONE place. The sampler container and the fetch-model
init container both include it, so the downloader that checks the model's
SHA-256 is always the one this release ships.
*/}}
{{- define "gawk-sampler.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end }}

{{/*
One `foo` / `fooRef` secret env var, the house dual form (gawk-admin's
relay.adminToken / adminTokenRef): Ref wins when both are set, and the literal
never renders beside a Ref.
  include "gawk-sampler.secretEnv" (dict "name" "ENV" "value" .lit "ref" .ref)
*/}}
{{- define "gawk-sampler.secretEnv" -}}
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
