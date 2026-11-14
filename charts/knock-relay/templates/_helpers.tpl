{{/* Chart name. */}}
{{- define "knock-relay.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Fully qualified app name. */}}
{{- define "knock-relay.fullname" -}}
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

{{- define "knock-relay.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "knock-relay.selectorLabels" -}}
app.kubernetes.io/name: {{ include "knock-relay.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "knock-relay.labels" -}}
helm.sh/chart: {{ include "knock-relay.chart" . }}
{{ include "knock-relay.selectorLabels" . }}
app.kubernetes.io/version: {{ .Values.image.tag | default .Chart.AppVersion | quote }}
app.kubernetes.io/component: relay
app.kubernetes.io/part-of: knock
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "knock-relay.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "knock-relay.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* image reference: repo@digest or repo:tag (tag defaults to appVersion). */}}
{{- define "knock-relay.image" -}}
{{- if .Values.image.digest }}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest }}
{{- else }}
{{- printf "%s:%s" .Values.image.repository (.Values.image.tag | default .Chart.AppVersion | toString) }}
{{- end }}
{{- end }}

{{/* Name of the chart-managed Secret. */}}
{{- define "knock-relay.secretName" -}}
{{- include "knock-relay.fullname" . }}
{{- end }}

{{/* "true" when the chart renders its own Secret (token secret or provider credentials from values). */}}
{{- define "knock-relay.needsSecret" -}}
{{- $a := .Values.auth -}}
{{- if or (not .Values.tokenSecret.existingSecret) (and $a.google.clientId (not $a.google.existingSecret)) (and $a.github.clientId (not $a.github.existingSecret)) -}}
true
{{- end -}}
{{- end }}

{{/* Number of configured sign-in providers. */}}
{{- define "knock-relay.providerCount" -}}
{{- $n := 0 -}}
{{- if or .Values.auth.google.clientId .Values.auth.google.existingSecret }}{{ $n = add1 $n }}{{ end -}}
{{- if or .Values.auth.github.clientId .Values.auth.github.existingSecret }}{{ $n = add1 $n }}{{ end -}}
{{- $n -}}
{{- end }}

{{/* allowAnonymous as "", "true" or "false" (unset stays empty, like knockd). */}}
{{- define "knock-relay.allowAnonymous" -}}
{{- $v := .Values.allowAnonymous -}}
{{- if not (kindIs "invalid" $v) -}}
{{- $s := toString $v | trim | lower -}}
{{- if or (eq $s "true") (eq $s "1") }}true{{ else if or (eq $s "false") (eq $s "0") }}false{{ else if ne $s "" }}{{ fail (printf "allowAnonymous=%q: want true, false or null" $s) }}{{ end -}}
{{- end -}}
{{- end }}

{{/* KNOCK_PUBLIC_URL: explicit, else from the first ingress host. Empty means knockd's default. */}}
{{- define "knock-relay.publicURL" -}}
{{- if .Values.publicURL -}}
{{- .Values.publicURL | trimSuffix "/" -}}
{{- else if and .Values.ingress.enabled .Values.ingress.hosts -}}
{{- printf "https://%s" (first .Values.ingress.hosts).host -}}
{{- end -}}
{{- end }}

{{/* Fails the render on settings knockd can't run with. */}}
{{- define "knock-relay.validate" -}}
{{- if ne (toString .Values.store.type) "memory" }}
{{- fail (printf "store.type=%q is not supported in knock-relay %s: only \"memory\" is. A shared store for several replicas is planned." (toString .Values.store.type) .Chart.AppVersion) }}
{{- end }}
{{- if ne (int .Values.replicaCount) 1 }}
{{- fail "replicaCount must be 1 with store.type=memory: relay state lives in one pod." }}
{{- end }}
{{- range $p := list "google" "github" }}
{{- $c := index $.Values.auth $p }}
{{- if and $c.clientId (not $c.clientSecret) (not $c.existingSecret) }}
{{- fail (printf "auth.%s.clientId is set without auth.%s.clientSecret (or auth.%s.existingSecret)." $p $p $p) }}
{{- end }}
{{- end }}
{{- $providers := include "knock-relay.providerCount" . | int }}
{{- if and (eq (include "knock-relay.allowAnonymous" .) "false") (eq $providers 0) }}
{{- fail "allowAnonymous=false needs a sign-in provider (auth.google or auth.github), or nobody can connect." }}
{{- end }}
{{- if and (gt $providers 0) (not (include "knock-relay.publicURL" .)) }}
{{- fail "publicURL is required with a sign-in provider: providers redirect to <publicURL>/v1/auth/{provider}/callback. Set publicURL or enable the ingress." }}
{{- end }}
{{- if and .Values.tokenSecret.value (lt (len .Values.tokenSecret.value) 32) }}
{{- fail "tokenSecret.value must be at least 32 bytes (openssl rand -base64 48)." }}
{{- end }}
{{- $g := toString .Values.drainGrace }}
{{- $drain := -1 }}
{{- if regexMatch "^[0-9]+s$" $g }}{{ $drain = trimSuffix "s" $g | int }}{{ end }}
{{- if regexMatch "^[0-9]+m$" $g }}{{ $drain = mul (trimSuffix "m" $g | int) 60 }}{{ end }}
{{- if and (ge $drain 0) (le (int .Values.terminationGracePeriodSeconds) $drain) }}
{{- fail (printf "terminationGracePeriodSeconds (%d) must be longer than drainGrace (%s), or the pod is killed mid-drain." (int .Values.terminationGracePeriodSeconds) $g) }}
{{- end }}
{{- end }}
