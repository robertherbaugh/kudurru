package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

type openAPIDoc struct {
	Info struct {
		Title   string `json:"title"`
		Version string `json:"version"`
	} `json:"info"`
	Servers []struct {
		URL string `json:"url"`
	} `json:"servers"`
	Paths map[string]map[string]openAPIOperation `json:"paths"`
}

type openAPIOperation struct {
	OperationID string   `json:"operationId"`
	Summary     string   `json:"summary"`
	Tags        []string `json:"tags"`
	// Permission is {resource, verb, scope} or the string "public" or
	// "authenticated".
	Permission json.RawMessage `json:"x-kudurru-permission"`
}

// permission renders the x-kudurru-permission extension.
func (op openAPIOperation) permission() string {
	if len(op.Permission) == 0 {
		return ""
	}
	var word string
	if json.Unmarshal(op.Permission, &word) == nil {
		return word
	}
	var p struct {
		Resource string `json:"resource"`
		Verb     string `json:"verb"`
		Scope    string `json:"scope"`
	}
	if json.Unmarshal(op.Permission, &p) != nil || p.Resource == "" {
		return ""
	}
	s := code(p.Resource + "." + p.Verb)
	if p.Scope != "" {
		s += " (" + p.Scope + ")"
	}
	return s
}

// methodOrder sorts the operations of one path the way a reader expects.
var methodOrder = map[string]int{"get": 0, "post": 1, "put": 2, "patch": 3, "delete": 4, "head": 5, "options": 6}

// endpointReference renders the operation index of the OpenAPI document,
// grouped by tag.
func endpointReference(file string) ([]byte, error) {
	b, err := os.ReadFile(file) //nolint:gosec // the repository's own OpenAPI document
	if err != nil {
		return nil, err
	}
	var doc openAPIDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	if len(doc.Paths) == 0 {
		return nil, fmt.Errorf("%s: no paths", file)
	}
	base := ""
	if len(doc.Servers) > 0 {
		base = strings.TrimSuffix(doc.Servers[0].URL, "/")
	}

	type row struct {
		method, path string
		op           openAPIOperation
	}
	groups := map[string][]row{}
	for path, ops := range doc.Paths {
		for method, op := range ops {
			if _, ok := methodOrder[method]; !ok {
				continue // "parameters" and other path-level keys
			}
			tag := "other"
			if len(op.Tags) > 0 {
				tag = op.Tags[0]
			}
			groups[tag] = append(groups[tag], row{method: method, path: path, op: op})
		}
	}
	tags := make([]string, 0, len(groups))
	for t := range groups {
		tags = append(tags, t)
	}
	sort.Strings(tags)

	var out strings.Builder
	out.WriteString(frontMatter("Endpoint index", "Every route of the REST API with the permission it checks.", 20))
	out.WriteString(generatedNote(openAPIFile))
	fmt.Fprintf(&out, "Every operation of %s %s, generated from `%s`. Paths include the\nbase path %s. The permission is the RBAC check of the route (`resource.verb` and the scope it\n"+
		"is evaluated in), `authenticated` (any signed-in caller) or `public` (no credentials).\nThe [API explorer](explorer.md) shows the request and response schemas.\n",
		doc.Info.Title, doc.Info.Version, openAPIFile, code(base))
	for _, t := range tags {
		rows := groups[t]
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].path != rows[j].path {
				return rows[i].path < rows[j].path
			}
			return methodOrder[rows[i].method] < methodOrder[rows[j].method]
		})
		fmt.Fprintf(&out, "\n## %s\n\n| Method | Path | Summary | Permission |\n|---|---|---|---|\n", t)
		for _, r := range rows {
			fmt.Fprintf(&out, "| %s | %s | %s | %s |\n", strings.ToUpper(r.method), code(base+r.path), text(r.op.Summary), r.op.permission())
		}
	}
	return []byte(out.String()), nil
}
