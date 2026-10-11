package api

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Took carries took_ms, the server-side duration of the request, in every
// non-object response body (§10: "every body carries took_ms").
// Kubernetes-shaped object bodies stay pure objects so they can be
// re-applied verbatim; their duration is in the Server-Timing header.
type Took struct {
	TookMS float64 `json:"took_ms" doc:"Server-side duration of the request in milliseconds."`
}

func (t *Took) setTook(ms float64) { t.TookMS = ms }

// tookSetter is implemented by bodies that carry took_ms.
type tookSetter interface{ setTook(ms float64) }

// tookTransformer stamps took_ms into bodies (success and error) and sets
// Server-Timing on every huma response.
func tookTransformer(ctx huma.Context, _ string, v any) (any, error) {
	ms := tookMS(ctx.Context())
	ctx.SetHeader("Server-Timing", "app;dur="+strconv.FormatFloat(ms, 'f', 3, 64))
	if t, ok := v.(tookSetter); ok {
		t.setTook(ms)
		return v, nil
	}
	// huma hands struct bodies over by value; stamp a pointer copy.
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.Struct {
		p := reflect.New(rv.Type())
		p.Elem().Set(rv)
		if t, ok := p.Interface().(tookSetter); ok {
			t.setTook(ms)
			return p.Interface(), nil
		}
	}
	return v, nil
}

// List is the list envelope {items, continue, took_ms}.
type List[T any] struct {
	Items []T `json:"items" nullable:"false"`
	// Continue fetches the next page; empty on the last page.
	Continue string `json:"continue,omitempty" doc:"Opaque keyset token for the next page; empty on the last page."`
	Took
}

// newList builds a list envelope with a non-nil Items slice.
func newList[T any](items []T, cont string) *List[T] {
	if items == nil {
		items = []T{}
	}
	return &List[T]{Items: items, Continue: cont}
}

// Processed is the result of the multi-item routes
// (POST /domains/..., :batchDelete): processed{success,errors}.
type Processed struct {
	Processed ProcessedItems `json:"processed"`
	Took
}

// ProcessedItems lists the items that succeeded and failed.
type ProcessedItems struct {
	Success []ProcessedItem `json:"success" nullable:"false"`
	Errors  []ProcessedItem `json:"errors" nullable:"false"`
}

// ProcessedItem is one item of a multi-item request.
type ProcessedItem struct {
	Item  string `json:"item"`
	Error string `json:"error,omitempty"`
}

func newProcessed() *Processed {
	return &Processed{Processed: ProcessedItems{Success: []ProcessedItem{}, Errors: []ProcessedItem{}}}
}

func (p *Processed) ok(item string) {
	p.Processed.Success = append(p.Processed.Success, ProcessedItem{Item: item})
}

func (p *Processed) fail(item string, err error) {
	p.Processed.Errors = append(p.Processed.Errors, ProcessedItem{Item: item, Error: err.Error()})
}

// encodeCursor and decodeCursor make keyset positions opaque.
func encodeCursor(pos string) string {
	if pos == "" {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(pos))
}

func decodeCursor(c string) (string, error) {
	if c == "" {
		return "", nil
	}
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return "", errInvalid("continue: not a token returned by this API")
	}
	return string(b), nil
}

// registerTypeAliases maps Kubernetes types whose JSON form differs from
// their Go structure onto the schema of their JSON form, so the OpenAPI
// document (and the clients generated from it) describe the wire.
func registerTypeAliases(r huma.Registry) {
	r.RegisterTypeAlias(reflect.TypeFor[metav1.Time](), reflect.TypeFor[time.Time]())
	r.RegisterTypeAlias(reflect.TypeFor[metav1.MicroTime](), reflect.TypeFor[time.Time]())
	r.RegisterTypeAlias(reflect.TypeFor[metav1.Duration](), reflect.TypeFor[string]())
	r.RegisterTypeAlias(reflect.TypeFor[metav1.FieldsV1](), reflect.TypeFor[map[string]any]())
	r.RegisterTypeAlias(reflect.TypeFor[intstr.IntOrString](), reflect.TypeFor[string]())
	r.RegisterTypeAlias(reflect.TypeFor[json.RawMessage](), reflect.TypeFor[any]())
}
