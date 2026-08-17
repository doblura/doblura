{{/* Base name, trimmed to 63 characters because of the label length limit. */}}
{{- define "doblura.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "doblura.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "doblura.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
selectorLabels: identity ONLY, no version.

A Deployment's spec.selector is immutable. If it carried the chart version, the
first `helm upgrade` would fail with "field is immutable" and you would have to
delete the Deployment by hand. This is bug number one in home-made charts.
*/}}
{{- define "doblura.selectorLabels" -}}
app.kubernetes.io/name: {{ include "doblura.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* labels: identity + version. For metadata, never for selectors. */}}
{{- define "doblura.labels" -}}
helm.sh/chart: {{ include "doblura.chart" . }}
{{ include "doblura.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/component: controller
{{- end -}}

{{- define "doblura.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "doblura.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "doblura.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}
