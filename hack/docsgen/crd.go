package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// samplesDir holds one example per kind (kudurru.io_<version>_<singular>.yaml).
const samplesDir = "config/samples"

// crdReference renders crds/_index.md and one page per CRD in dir.
func crdReference(dir string) ([]page, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	if len(files) == 0 {
		return nil, fmt.Errorf("no CRDs in %s", dir)
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(dir))) // config/crd/bases → repository root

	var index strings.Builder
	index.WriteString(frontMatter("CRD reference", "The kudurru.io custom resources: every field, its type and its validation.", 20))
	index.WriteString(generatedNote(crdBases))
	index.WriteString("The `kudurru.io` kinds are the same documents in every deployment shape:\n" +
		"CRDs in Kubernetes (`kubectl apply`), and the objects of the sqlite store in a\n" +
		"single container (`kudurru apply -f`, `objects.d/`, the API and the UI). The\n" +
		"field tables are generated from the OpenAPI v3 schemas of the CRDs, so the\n" +
		"validation listed here is what the apiserver, and kudurru's own store\n" +
		"validator in Docker mode, enforce.\n\n")
	index.WriteString("| Kind | Scope | Short names | Description |\n|---|---|---|---|\n")

	var pages []page
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // the repository's own CRDs
		if err != nil {
			return nil, err
		}
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.UnmarshalStrict(b, &crd); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		name := strings.ToLower(crd.Spec.Names.Kind) + ".md"
		body, summary, err := crdPage(root, &crd)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		pages = append(pages, page{Path: crdDir + "/" + name, Body: body})
		fmt.Fprintf(&index, "| [%s](%s) | %s | %s | %s |\n", crd.Spec.Names.Kind, name,
			crd.Spec.Scope, code(strings.Join(crd.Spec.Names.ShortNames, ", ")), text(summary))
	}
	return append([]page{{Path: crdDir + "/_index.md", Body: []byte(index.String())}}, pages...), nil
}

// crdPage renders one kind and returns its one-line summary.
func crdPage(root string, crd *apiextensionsv1.CustomResourceDefinition) ([]byte, string, error) {
	names := crd.Spec.Names
	var b strings.Builder
	var summary string
	for _, v := range crd.Spec.Versions {
		if v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
			return nil, "", fmt.Errorf("version %s has no schema", v.Name)
		}
		if summary == "" {
			summary = firstSentence(v.Schema.OpenAPIV3Schema.Description)
		}
	}
	b.WriteString(frontMatter(names.Kind, summary, 0))
	b.WriteString(generatedNote(crdBases + "/" + crd.Spec.Group + "_" + names.Plural + ".yaml"))
	for _, v := range crd.Spec.Versions {
		s := v.Schema.OpenAPIV3Schema
		if len(crd.Spec.Versions) > 1 {
			fmt.Fprintf(&b, "## %s\n\n", v.Name)
		}
		// The first sentence is the page description (the lead on the
		// site); repeat the description only when it says more.
		if d := text(s.Description); d != text(summary) {
			b.WriteString(d + "\n\n")
		}
		b.WriteString("| | |\n|---|---|\n")
		fmt.Fprintf(&b, "| API version | %s |\n", code(crd.Spec.Group+"/"+v.Name))
		fmt.Fprintf(&b, "| Kind | %s |\n", code(names.Kind))
		fmt.Fprintf(&b, "| Scope | %s |\n", crd.Spec.Scope)
		fmt.Fprintf(&b, "| Resource | %s |\n", code(names.Plural+"."+crd.Spec.Group))
		if len(names.ShortNames) > 0 {
			fmt.Fprintf(&b, "| Short names | %s |\n", code(strings.Join(names.ShortNames, ", ")))
		}
		if v.Subresources != nil && v.Subresources.Status != nil {
			b.WriteString("| Subresources | `status` |\n")
		}
		b.WriteString("\n")

		if len(v.AdditionalPrinterColumns) > 0 {
			fmt.Fprintf(&b, "Columns of `kubectl get %s`:\n\n", names.Plural)
			b.WriteString("| Column | Type | Value |\n|---|---|---|\n")
			for _, c := range v.AdditionalPrinterColumns {
				fmt.Fprintf(&b, "| %s | %s | %s |\n", c.Name, c.Type, code(c.JSONPath))
			}
			b.WriteString("\n")
		}

		for _, part := range []string{"spec", "status"} {
			prop, ok := s.Properties[part]
			if !ok {
				continue
			}
			title := strings.ToUpper(part[:1]) + part[1:]
			fmt.Fprintf(&b, "## %s\n\n", title)
			if prop.Description != "" {
				b.WriteString(text(prop.Description) + "\n\n")
			}
			if c := constraints(prop); c != "" {
				// Outside a table a pipe needs no escape (and inside a
				// code span the backslash would show).
				c = strings.ReplaceAll(strings.ReplaceAll(c, `\|`, "|"), "<br>", "\n\n")
				b.WriteString(c + "\n\n")
			}
			b.WriteString("| Field | Type | Description |\n|---|---|---|\n")
			children(&b, part, prop)
			b.WriteString("\n")
		}

		sample := filepath.Join(root, samplesDir, crd.Spec.Group+"_"+v.Name+"_"+names.Singular+".yaml")
		if ex, err := os.ReadFile(sample); err == nil { //nolint:gosec // the repository's own samples
			fmt.Fprintf(&b, "## Example\n\n`%s/%s`:\n\n```yaml\n%s```\n", samplesDir, filepath.Base(sample), ex)
		}
	}
	return []byte(b.String()), summary, nil
}

// fields writes one row for path and recurses into object properties and
// array items.
func fields(b *strings.Builder, path string, s apiextensionsv1.JSONSchemaProps, isRequired bool) {
	desc := text(s.Description)
	if isRequired {
		desc = strings.TrimSpace("**Required.** " + desc)
	}
	if c := constraints(s); c != "" {
		if desc != "" {
			desc += "<br>"
		}
		desc += c
	}
	fmt.Fprintf(b, "| %s | %s | %s |\n", code(path), code(typeName(s)), desc)
	children(b, path, s)
}

// children writes the rows below path: object properties, array item
// properties and map value properties.
func children(b *strings.Builder, path string, s apiextensionsv1.JSONSchemaProps) {
	if isCondition(s) {
		return
	}
	switch {
	case s.Type == "array" && s.Items != nil && s.Items.Schema != nil:
		item := *s.Items.Schema
		for _, name := range sortedKeys(item.Properties) {
			fields(b, path+"[]."+name, item.Properties[name], slices.Contains(item.Required, name))
		}
	case len(s.Properties) > 0:
		for _, name := range sortedKeys(s.Properties) {
			fields(b, path+"."+name, s.Properties[name], slices.Contains(s.Required, name))
		}
	case s.AdditionalProperties != nil && s.AdditionalProperties.Schema != nil:
		v := *s.AdditionalProperties.Schema
		for _, name := range sortedKeys(v.Properties) {
			fields(b, path+".<key>."+name, v.Properties[name], slices.Contains(v.Required, name))
		}
	}
}

func typeName(s apiextensionsv1.JSONSchemaProps) string {
	switch {
	case s.XIntOrString:
		return "int or string"
	case isCondition(s):
		return "[]Condition"
	case s.Type == "array" && s.Items != nil && s.Items.Schema != nil:
		return "[]" + typeName(*s.Items.Schema)
	case s.Type == "object" && s.AdditionalProperties != nil && s.AdditionalProperties.Schema != nil:
		return "map[string]" + typeName(*s.AdditionalProperties.Schema)
	case s.Type == "object" && len(s.Properties) == 0:
		return "object (free-form)"
	case s.Type == "":
		return "any"
	}
	return s.Type
}

// constraints lists the schema's validation in words.
func constraints(s apiextensionsv1.JSONSchemaProps) string {
	var c []string
	if s.Default != nil {
		var v any
		if err := json.Unmarshal(s.Default.Raw, &v); err != nil {
			v = string(s.Default.Raw)
		}
		c = append(c, "Default: "+code(flowValue(v)))
	}
	if len(s.Enum) > 0 {
		vals := make([]string, len(s.Enum))
		for i, e := range s.Enum {
			vals[i] = code(strings.Trim(string(e.Raw), `"`))
		}
		c = append(c, "One of: "+strings.Join(vals, ", "))
	}
	if s.Format != "" {
		c = append(c, "Format: "+code(s.Format))
	}
	if s.Pattern != "" {
		c = append(c, "Pattern: "+code(s.Pattern))
	}
	c = appendRange(c, "Length", s.MinLength, s.MaxLength)
	c = appendRange(c, "Items", s.MinItems, s.MaxItems)
	c = appendRange(c, "Properties", s.MinProperties, s.MaxProperties)
	if s.Minimum != nil || s.Maximum != nil {
		var lo, hi *int64
		if s.Minimum != nil {
			v := int64(*s.Minimum)
			lo = &v
		}
		if s.Maximum != nil {
			v := int64(*s.Maximum)
			hi = &v
		}
		c = appendRange(c, "Range", lo, hi)
	}
	if s.XListType != nil && *s.XListType != "atomic" {
		l := "List type: " + code(*s.XListType)
		if len(s.XListMapKeys) > 0 {
			l += ", keyed by " + code(strings.Join(s.XListMapKeys, ", "))
		}
		c = append(c, l)
	}
	for _, v := range s.XValidations {
		msg := v.Message
		if msg == "" {
			msg = "must satisfy " + v.Rule
		}
		c = append(c, "Validation: "+text(msg)+" ("+code(strings.Join(strings.Fields(v.Rule), " "))+")")
	}
	return strings.Join(c, "<br>")
}

func appendRange(c []string, what string, lo, hi *int64) []string {
	switch {
	case lo != nil && hi != nil:
		return append(c, fmt.Sprintf("%s: %d to %d", what, *lo, *hi))
	case lo != nil:
		return append(c, fmt.Sprintf("%s: at least %d", what, *lo))
	case hi != nil:
		return append(c, fmt.Sprintf("%s: at most %d", what, *hi))
	}
	return c
}

// isCondition recognises a metav1.Condition list, which is documented
// once by Kubernetes instead of field by field here.
func isCondition(s apiextensionsv1.JSONSchemaProps) bool {
	if s.Type != "array" || s.Items == nil || s.Items.Schema == nil {
		return false
	}
	p := s.Items.Schema.Properties
	for _, k := range []string{"type", "status", "reason", "message", "lastTransitionTime"} {
		if _, ok := p[k]; !ok {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]apiextensionsv1.JSONSchemaProps) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// firstSentence is the description up to its first full stop.
func firstSentence(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if i := strings.Index(s, ". "); i >= 0 {
		return s[:i+1]
	}
	return s
}
