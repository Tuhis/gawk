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
GAWK_ADMIN_PG_DSN — the connection to a database this chart does NOT create.

The database is a PREREQUISITE of the release, not a part of it: gawk-admin is
stateless and horizontally scaled (D16), Postgres is stateful and outlives
every one of its releases, and `helm uninstall` must never be able to take the
audit trail with it. So the chart takes a connection, in the house `foo` /
`fooRef` dual form (gawk-server's internalPsk/internalPskRef precedent):

  postgres.dsn                   a literal DSN, and
  postgres.dsnSecretRef {name,key}  a Secret holding one — Ref wins if both.

CloudNativePG is then not a special case at all, just the ordinary Ref form:
CNPG generates <cluster>-app for its application user and that Secret's `uri`
key IS this DSN, so `dsnSecretRef: {name: gawk-admin-db-app, key: uri}` is the
entire integration. (`<cluster>-app`/`uri` is the operator's own contract, not
a name anybody here chooses.)

Shared by the Deployment and the migrate Job so the two can never drift onto
different databases.
*/}}
{{- define "gawk-admin.pgDsnEnv" -}}
{{- $ref := .Values.postgres.dsnSecretRef | default dict -}}
{{- if and $ref (not $ref.name) -}}
{{- fail "postgres.dsnSecretRef needs a `name`: it is {name, key} naming a Secret that holds the Postgres DSN (docs/42 §4.13)" -}}
{{- end -}}
{{- if $ref.name -}}
- name: GAWK_ADMIN_PG_DSN
  valueFrom:
    secretKeyRef:
      name: {{ $ref.name | quote }}
      key: {{ $ref.key | default "dsn" | quote }}
{{- else if .Values.postgres.dsn -}}
- name: GAWK_ADMIN_PG_DSN
  value: {{ .Values.postgres.dsn | quote }}
{{- else -}}
{{- fail "gawk-admin needs a Postgres it does not create: set postgres.dsn (a literal DSN) or postgres.dsnSecretRef {name, key} (a Secret holding one). The database is a prerequisite of this release, not part of it, and it must exist BEFORE `helm install` — the pre-install migration hook Job connects to it. For a CloudNativePG cluster named gawk-admin-db, CNPG's generated Secret gawk-admin-db-app carries the DSN under `uri`: postgres.dsnSecretRef: {name: gawk-admin-db-app, key: uri}. See docs/42 §4.13 and docs/self-hosting.md §9.1." -}}
{{- end -}}
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
