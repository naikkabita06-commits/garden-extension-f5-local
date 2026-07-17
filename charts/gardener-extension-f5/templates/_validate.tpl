{{- define "gardener-extension-f5.validate" -}}
{{- /*
Fail fast on invalid inputs.
*/ -}}
{{- if gt (len .Release.Namespace) 63 -}}
{{- fail (printf "Release namespace '%s' is %d chars; Kubernetes namespaces must be <= 63 chars." .Release.Namespace (len .Release.Namespace)) -}}
{{- end -}}
{{- end -}}
