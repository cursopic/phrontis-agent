{{- define "phrontis-agent.labels" -}}
app.kubernetes.io/name: phrontis-agent
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "phrontis-agent.secret" -}}
{{- .Values.api.existingSecret | default (printf "%s-key" .Release.Name) }}
{{- end }}
