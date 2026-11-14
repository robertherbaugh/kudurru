// Command clientdispatch generates the CLI's dispatch to the actual
// oapi-codegen operations. Run after regenerating the OpenAPI client.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"slices"
	"strings"
)

type operation struct {
	Method, Path string
}

type method struct {
	name   string
	params []*ast.Field
}

var correspondence = regexp.MustCompile("Corresponds with ([A-Z]+) ([^ ]+) \\(the `[^`]+` operationId\\)")

func main() {
	spec := flag.String("spec", "api/openapi-3.0.json", "OpenAPI document")
	client := flag.String("client", "internal/apiclient/openapi/zz_generated.client.go", "oapi-codegen client")
	out := flag.String("out", "internal/apiclient/openapi/zz_generated.dispatch.go", "output file")
	flag.Parse()
	if err := run(*spec, *client, *out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(spec, client, out string) error {
	schema, err := os.ReadFile(spec) //nolint:gosec // explicit generator input
	if err != nil {
		return err
	}
	source, err := os.ReadFile(client) //nolint:gosec // explicit generator input
	if err != nil {
		return err
	}
	data, count, err := generate(schema, source)
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, data, 0o600); err != nil { //nolint:gosec // explicit generator output
		return err
	}
	fmt.Printf("generated dispatch for %d OpenAPI operations\n", count)
	return nil
}

func generate(schema, source []byte) ([]byte, int, error) {
	var spec struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(schema, &spec); err != nil {
		return nil, 0, err
	}
	var ops []operation
	for path, item := range spec.Paths {
		for verb := range item {
			switch verb {
			case "get", "put", "post", "delete", "options", "head", "patch", "trace":
				ops = append(ops, operation{strings.ToUpper(verb), path})
			}
		}
	}
	if len(ops) == 0 {
		return nil, 0, fmt.Errorf("no OpenAPI operations")
	}
	file, err := parser.ParseFile(token.NewFileSet(), "client.go", source, parser.ParseComments)
	if err != nil {
		return nil, 0, err
	}
	methods := map[operation]method{}
	types := map[string]string{}
	for _, decl := range file.Decls {
		if group, ok := decl.(*ast.GenDecl); ok {
			for _, item := range group.Specs {
				if typ, ok := item.(*ast.TypeSpec); ok {
					if underlying, ok := typ.Type.(*ast.Ident); ok {
						types[typ.Name.Name] = underlying.Name
					}
				}
			}
		}
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Doc == nil {
			continue
		}
		receiver, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		name, ok := receiver.X.(*ast.Ident)
		if !ok || name.Name != "Client" {
			continue
		}
		match := correspondence.FindStringSubmatch(fn.Doc.Text())
		if match == nil {
			continue
		}
		op := operation{match[1], match[2]}
		// Raw request bodies preserve the CLI's JSON/YAML and streaming API.
		// Prefer the generated WithBody variant over its JSON model helper.
		previous, exists := methods[op]
		if !exists || strings.HasSuffix(fn.Name.Name, "WithBody") {
			methods[op] = method{fn.Name.Name, fn.Type.Params.List}
		} else if !strings.HasSuffix(previous.name, "WithBody") {
			return nil, 0, fmt.Errorf("ambiguous generated operation %s %s", op.Method, op.Path)
		}
	}
	if len(methods) != len(ops) {
		return nil, 0, fmt.Errorf("OpenAPI/client operation count mismatch: %d/%d; regenerate client first", len(ops), len(methods))
	}
	// Static segments outrank parameters. Thus /zones/options cannot be
	// dispatched to /zones/{zone}; the request editor never changes paths.
	slices.SortFunc(ops, func(a, b operation) int {
		as, bs := strings.Split(a.Path, "/"), strings.Split(b.Path, "/")
		for i := 0; i < min(len(as), len(bs)); i++ {
			ap, bp := strings.HasPrefix(as[i], "{"), strings.HasPrefix(bs[i], "{")
			if ap != bp {
				if ap {
					return 1
				}
				return -1
			}
			if !ap && as[i] != bs[i] {
				return strings.Compare(as[i], bs[i])
			}
		}
		if len(as) != len(bs) {
			return len(as) - len(bs)
		}
		return strings.Compare(a.Path+" "+a.Method, b.Path+" "+b.Method)
	})
	var out bytes.Buffer
	fmt.Fprint(&out, dispatchPreamble)
	for _, op := range ops {
		m, ok := methods[op]
		if !ok {
			return nil, 0, fmt.Errorf("missing generated operation %s %s", op.Method, op.Path)
		}
		if err := emitOperation(&out, op, m, types); err != nil {
			return nil, 0, err
		}
	}
	fmt.Fprintln(&out, "return nil, false, nil\n}")
	generated := out.String()
	if !strings.Contains(generated, "strconv.ParseInt") {
		generated = strings.Replace(generated, " \"strconv\"\n", "", 1)
	}
	data, err := format.Source([]byte(generated))
	return data, len(ops), err
}

func emitOperation(out *bytes.Buffer, op operation, m method, types map[string]string) error {
	parts := strings.Split(strings.TrimPrefix(op.Path, "/"), "/")
	condition := fmt.Sprintf("method == %q && len(parts) == %d", op.Method, len(parts))
	var indices []int
	for i, part := range parts {
		if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") {
			indices = append(indices, i)
		} else {
			if strings.ContainsAny(part, "{}") {
				return fmt.Errorf("unsupported partial path parameter: %s", op.Path)
			}
			condition += fmt.Sprintf(" && parts[%d] == %q", i, part)
		}
	}
	var setup bytes.Buffer
	args := []string{"ctx"}
	index := 0
	for _, field := range m.params {
		for _, name := range field.Names {
			switch name.Name {
			case "ctx":
			case "params":
				args = append(args, "nil")
			case "contentType", "body":
				args = append(args, name.Name)
			case "reqEditors":
				args = append(args, "editors...")
			default:
				if index >= len(indices) {
					return fmt.Errorf("unexpected parameter %s in %s", name.Name, m.name)
				}
				typ, ok := field.Type.(*ast.Ident)
				if !ok {
					return fmt.Errorf("unsupported path parameter in %s", m.name)
				}
				value := fmt.Sprintf("parts[%d]", indices[index])
				underlying := typ.Name
				if base, ok := types[underlying]; ok {
					underlying = base
				}
				convert := func(value string) string {
					if underlying != typ.Name {
						return typ.Name + "(" + value + ")"
					}
					return value
				}
				switch underlying {
				case "string":
					args = append(args, convert(value))
				case "int64":
					variable := fmt.Sprintf("p%d", index)
					fmt.Fprintf(&setup, "%s, err := strconv.ParseInt(%s, 10, 64)\nif err != nil { return nil, true, fmt.Errorf(\"invalid integer path parameter: %%w\", err) }\n", variable, value)
					args = append(args, convert(variable))
				default:
					return fmt.Errorf("unsupported path parameter type %s in %s", typ.Name, m.name)
				}
				index++
			}
		}
	}
	if index != len(indices) {
		return fmt.Errorf("path parameter count mismatch in %s", m.name)
	}
	fmt.Fprintf(out, "if %s {\n%sresponse, err := c.%s(%s)\nreturn response, true, err\n}\n", condition, setup.String(), m.name, strings.Join(args, ", "))
	return nil
}

const dispatchPreamble = `// Code generated by hack/clientdispatch from OpenAPI and oapi-codegen; DO NOT EDIT.
package openapi

import (
 "context"
 "fmt"
 "io"
 "net/http"
 "net/url"
 "strconv"
 "strings"
)

// Dispatch selects the matching generated operation for an escaped path
// relative to the client's API base. It never substitutes a different
// operation or rewrites a path. Query and headers may be supplied by editors.
// An unknown method/path returns matched=false without a request; the caller
// decides whether an undocumented route is allowed. A matched operation's
// errors are final, never permission to fall back or retry.
func (c *Client) Dispatch(ctx context.Context, method, path, contentType string, body io.Reader, editors ...RequestEditorFn) (response *http.Response, matched bool, err error) {
 parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
 for i := range parts {
  parts[i], err = url.PathUnescape(parts[i])
  if err != nil { return nil, true, fmt.Errorf("invalid API path escape: %w", err) }
 }
`
