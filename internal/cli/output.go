package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"sigs.k8s.io/yaml"
)

// Format is an output format selected with -o.
type Format string

// Output formats.
const (
	FormatTable Format = "table"
	FormatWide  Format = "wide"
	FormatJSON  Format = "json"
	FormatYAML  Format = "yaml"
	FormatName  Format = "name"
)

// Formats lists the accepted -o values.
var Formats = []string{"table", "wide", "json", "yaml", "name"}

// ParseFormat validates an -o value; "" means table.
func ParseFormat(s string) (Format, error) {
	switch Format(strings.ToLower(s)) {
	case "", FormatTable:
		return FormatTable, nil
	case FormatWide:
		return FormatWide, nil
	case FormatJSON:
		return FormatJSON, nil
	case FormatYAML, "yml":
		return FormatYAML, nil
	case FormatName:
		return FormatName, nil
	}
	return "", fmt.Errorf("unknown output format %q (use %s)", s, strings.Join(Formats, ", "))
}

// Structured reports whether f is json or yaml.
func (f Format) Structured() bool { return f == FormatJSON || f == FormatYAML }

// WriteStructured writes v as indented JSON or YAML. json.RawMessage and
// []json.RawMessage values are re-encoded, not quoted.
func WriteStructured(w io.Writer, f Format, v any) error {
	switch f {
	case FormatYAML:
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		y, err := yaml.JSONToYAML(b)
		if err != nil {
			return err
		}
		_, err = w.Write(y)
		return err
	default:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		return enc.Encode(v)
	}
}

// Table is a kubectl-style table: upper-case headers, columns separated by
// three spaces, no borders.
type Table struct {
	Headers []string
	Rows    [][]string
}

// Add appends a row.
func (t *Table) Add(cells ...string) { t.Rows = append(t.Rows, cells) }

// Write renders the table. An empty table writes nothing (callers print
// "No resources found" themselves).
func (t *Table) Write(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 8, 3, ' ', 0)
	if len(t.Headers) > 0 {
		if _, err := fmt.Fprintln(tw, strings.Join(t.Headers, "\t")); err != nil {
			return err
		}
	}
	for _, r := range t.Rows {
		cells := make([]string, len(r))
		for i, c := range r {
			// Tabs or newlines inside a cell would break the layout.
			cells[i] = strings.NewReplacer("\t", " ", "\n", " ").Replace(c)
		}
		if _, err := fmt.Fprintln(tw, strings.Join(cells, "\t")); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// KeyValues renders aligned "Key:  value" lines (describe-style).
func KeyValues(w io.Writer, indent string, pairs [][2]string) error {
	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	for _, p := range pairs {
		if _, err := fmt.Fprintf(tw, "%s%s:\t%s\n", indent, p[0], p[1]); err != nil {
			return err
		}
	}
	return tw.Flush()
}
