package vector

import (
	_ "embed"
	"bytes"
	"crypto/sha256"
	"fmt"
	"net/url"
	"text/template"

	"github.com/stackit-mock/operator/api/v1alpha1"
)

// vectorConfigTmpl is the Vector TOML config template, embedded at compile time
// from vector.toml in the same package directory.
//
// Signal routing (Vector 0.51.1+):
//   - Logs    → enrich (VRL) → LOKI / OPENSEARCH sinks
//   - Traces  → opentelemetry sink ([encoding] codec=otlp) → Tempo
//   - Metrics → opentelemetry sink ([encoding] codec=otlp) → Prometheus OTLP receiver
//   - Vector pipeline metrics → prometheus_remote_write sink
//
// Supported Destination types:
//   LOKI        → loki sink (enriched logs)
//   OPENSEARCH  → elasticsearch sink, OpenSearch-compatible (enriched logs)
//   PROMETHEUS  → prometheus_remote_write sink (Vector internal metrics)
//   OTLP        → opentelemetry sink (traces)
//
// NOTE: Go template delimiters are [[ ]] so Vector's own {{ }} label templates
// can appear literally in the output without escaping.

//go:embed vector.toml
var vectorConfigTmpl string

type configData struct {
	Name         string
	Namespace    string
	Transforms   []v1alpha1.VRLTransform
	Destinations []v1alpha1.Destination
}

// sanitize makes a string safe for use as a TOML key.
func sanitize(s string) string {
	out := make([]byte, 0, len(s))
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			out = append(out, byte(c))
		} else {
			out = append(out, '_')
		}
	}
	return string(out)
}

// prometheusBase strips the path from a Prometheus remote_write endpoint URL
// and returns just the scheme+host, used to derive the OTLP receiver endpoint.
// e.g. "http://prometheus:9090/api/v1/write" → "http://prometheus:9090"
func prometheusBase(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	return fmt.Sprintf("%s://%s", u.Scheme, u.Host)
}

// Render produces the Vector TOML config string from a TelemetryRouter spec.
func Render(tr *v1alpha1.TelemetryRouter) (string, error) {
	funcMap := template.FuncMap{
		"sanitize":       sanitize,
		"prometheusBase": prometheusBase,
	}

	// Use [[ ]] as Go template delimiters so Vector's own {{ }} template syntax
	// (used in Loki sink label values) can appear literally in the output.
	tmpl, err := template.New("vector").Delims("[[", "]]").Funcs(funcMap).Parse(vectorConfigTmpl)
	if err != nil {
		return "", fmt.Errorf("parsing vector config template: %w", err)
	}

	data := configData{
		Name:         tr.Name,
		Namespace:    tr.Namespace,
		Transforms:   tr.Spec.Transforms,
		Destinations: tr.Spec.Destinations,
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("executing vector config template: %w", err)
	}

	return buf.String(), nil
}

// Hash returns a short content hash of the rendered config.
// The operator stores this in the CRD status to detect drift.
func Hash(config string) string {
	sum := sha256.Sum256([]byte(config))
	return fmt.Sprintf("%x", sum[:6])
}
