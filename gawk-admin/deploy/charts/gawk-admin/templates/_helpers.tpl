{{- define "gawk-admin.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end }}

{{- define "gawk-admin.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "gawk-admin.selectorLabels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "gawk-admin.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "gawk-admin.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end }}

{{/*
The image reference, in ONE place. Both the Deployment and the migration hook
Job include it, which is what makes docs/42 §4.15's guarantee mechanical rather
than aspirational: the schema that runs is the schema this release carries.
A hook that floated on :latest could migrate a database a version ahead of the
pods about to serve it.
*/}}
{{- define "gawk-admin.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end }}

{{/*
The CNPG Cluster's name, and the Secret CNPG generates for its application
user. The `-app` suffix is CNPG's own convention (it creates
<cluster>-app holding uri/username/password/dbname), so this is a read of the
operator's contract, not a name we choose.
*/}}
{{- define "gawk-admin.pgClusterName" -}}
{{ include "gawk-admin.fullname" . }}-pg
{{- end }}

{{- define "gawk-admin.pgSecretName" -}}
{{ include "gawk-admin.pgClusterName" . }}-app
{{- end }}

{{/*
GAWK_ADMIN_PG_DSN, wired either from the Secret CNPG generated or from the
operator's own dsnSecretRef. Shared by the Deployment and the migrate Job so
the two can never drift onto different databases.
*/}}
{{- define "gawk-admin.pgDsnEnv" -}}
- name: GAWK_ADMIN_PG_DSN
  valueFrom:
    secretKeyRef:
      {{- if .Values.postgres.cnpg.enabled }}
      name: {{ include "gawk-admin.pgSecretName" . | quote }}
      # CNPG writes a complete connection URI under `uri`.
      key: uri
      {{- else }}
      {{- if not .Values.postgres.dsnSecretRef.name }}
      {{- fail "postgres.cnpg.enabled=false requires postgres.dsnSecretRef: {name, key} naming a Secret that holds a Postgres DSN (docs/42 §4.13)" }}
      {{- end }}
      name: {{ .Values.postgres.dsnSecretRef.name | quote }}
      key: {{ .Values.postgres.dsnSecretRef.key | default "dsn" | quote }}
      {{- end }}
{{- end }}

{{/*
The environment variable that carries one chart-defined webhook's HMAC signing
key. Derived from the webhook's name so it is stable across renders and
readable in `kubectl describe pod` — the VALUE never appears in the rendered
manifest when a secretRef is used, and the name is all -static-webhooks
carries either way (docs/42 §4.12).

Anything that is not [A-Za-z0-9] becomes an underscore: webhook names are free
text ("ops-pager", "matrix room"), environment variable names are not.
*/}}
{{- define "gawk-admin.webhookSecretEnv" -}}
GAWK_ADMIN_WEBHOOK_SECRET_{{ regexReplaceAll "[^A-Za-z0-9]" . "_" | upper }}
{{- end }}

{{/*
notifications.webhooks -> the -static-webhooks JSON knob. The shape is
config.parseStaticWebhooks': [{"name","url","secretEnv"[,"enabled"]}]. No
`secret` key exists in it, and that is the point — a signing key in a values
file is a signing key in every GitOps diff.
*/}}
{{- define "gawk-admin.staticWebhooksJson" -}}
{{- $entries := list -}}
{{- range .Values.notifications.webhooks -}}
{{- if not .name }}{{ fail "notifications.webhooks[]: name is required" }}{{ end -}}
{{- if not .url }}{{ printf "notifications.webhooks[%s]: url is required" .name | fail }}{{ end -}}
{{- if and (not .secret) (not .secretRef) }}{{ printf "notifications.webhooks[%s]: set secret or secretRef {name, key} — the signing key is what makes a delivery verifiable" .name | fail }}{{ end -}}
{{- $entry := dict "name" .name "url" .url "secretEnv" (include "gawk-admin.webhookSecretEnv" .name) -}}
{{- if hasKey . "enabled" -}}
{{- $_ := set $entry "enabled" .enabled -}}
{{- end -}}
{{- $entries = append $entries $entry -}}
{{- end -}}
{{- toJson $entries -}}
{{- end }}
