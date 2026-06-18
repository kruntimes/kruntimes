package krt

import (
	"encoding/json"
	"fmt"
	"io"

	"sigs.k8s.io/yaml"
)

const (
	outputTable = "table"
	outputJSON  = "json"
	outputYAML  = "yaml"
)

func writeStructuredOutput(w io.Writer, format string, value any) error {
	switch format {
	case "", outputTable:
		return nil
	case outputJSON:
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		return encoder.Encode(value)
	case outputYAML:
		data, err := yaml.Marshal(value)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	default:
		return fmt.Errorf("unsupported output format %q", format)
	}
}
