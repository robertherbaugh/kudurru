package host

import (
	"encoding"
	"encoding/json"
	"reflect"
	"time"
	"unicode/utf8"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/inventory"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const maxObservationBytes = 32 << 20

// checkSourceBounds inspects the typed graph without marshaling/copying it or
// invoking arbitrary marshalers. Root's pure validator adds semantic admission.
func checkSourceBounds(s SourceSnapshot, preferred bool) error {
	maxClients := 100000
	if preferred {
		maxClients = kinds.MaxSourceClients
	}
	if len(s.Clients) > maxClients {
		return ErrCapacity
	}
	if d := s.Desired; d != nil && (len(d.Zones) > kinds.MaxSourceZones || len(d.RecordSets) > kinds.MaxSourceRecordSets || len(d.ClientGroups) > kinds.MaxSourceGroups) {
		return ErrCapacity
	}
	// Includes zoned-IP rejection before MarshalText and all escaped strings,
	// nested address/tag counts and original time bounds before allocation.
	state := inventory.State{FormatVersion: inventory.StateFormatVersion, Generation: 1, Sources: []inventory.SourceState{{ID: "observation", Revision: 1, Clients: s.Clients}}}
	if err := inventory.CheckStateBounds(state, maxObservationBytes); err != nil {
		return ErrCapacity
	}
	b := sourceBudget{}
	if err := b.value(reflect.ValueOf(s.Desired), 0); err != nil {
		return err
	}
	// Charge clients against the SAME aggregate before any defensive copy.
	return b.value(reflect.ValueOf(s.Clients), 0)
}

type sourceBudget struct{ bytes int }

func (b *sourceBudget) add(n int) error {
	if n < 0 || n > maxObservationBytes-b.bytes {
		return ErrCapacity
	}
	b.bytes += n
	return nil
}
func (b *sourceBudget) str(s string) error {
	if len(s) > 8192 || !utf8.ValidString(s) {
		return ErrCapacity
	}
	// Worst-case JSON escaping, including HTML/control bytes, without encoding.
	return b.add(2 + 6*len(s))
}
func (b *sourceBudget) value(v reflect.Value, depth int) error {
	if depth > 32 {
		return ErrCapacity
	}
	if v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return b.add(4)
		}
		return b.value(v.Elem(), depth+1)
	}
	if v.CanInterface() {
		switch leaf := v.Interface().(type) {
		case time.Time, metav1.Time, metav1.MicroTime:
			return b.add(64)
		case metav1.FieldsV1:
			// GetRawReader reports bytes without allocating a copy. Desired
			// objects may carry raw managed fields even though host ignores them.
			n := leaf.GetRawReader().Size()
			if n > 64<<10 {
				return ErrCapacity
			}
			return b.add(6*int(n) + 4)
		case json.RawMessage:
			if len(leaf) > 64<<10 {
				return ErrCapacity
			}
			return b.add(6*len(leaf) + 4)
		}
		// No arbitrary custom marshalers, including hidden large strings.
		if _, ok := v.Interface().(json.Marshaler); ok {
			return ErrCapacity
		}
		if _, ok := v.Interface().(encoding.TextMarshaler); ok {
			// Inventory IPs are checked independently before this walk. They
			// have a fixed <=64-byte representation and cannot carry a zone.
			if v.Type().PkgPath() == "net/netip" && v.Type().Name() == "Addr" {
				return b.add(64)
			}
			return ErrCapacity
		}
	}
	switch v.Kind() {
	case reflect.String:
		return b.str(v.String())
	case reflect.Bool:
		return b.add(5)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
		return b.add(32)
	case reflect.Slice, reflect.Array:
		if v.Len() > 100000 {
			return ErrCapacity
		}
		if err := b.add(4 + v.Len()); err != nil {
			return err
		}
		for i := 0; i < v.Len(); i++ {
			if err := b.value(v.Index(i), depth+1); err != nil {
				return err
			}
		}
	case reflect.Map:
		if v.Len() > kinds.MaxSourceRecordSets || v.Type().Key().Kind() != reflect.String {
			return ErrCapacity
		}
		if err := b.add(4 + v.Len()); err != nil {
			return err
		}
		iter := v.MapRange()
		for iter.Next() {
			if err := b.str(iter.Key().String()); err != nil {
				return err
			}
			if err := b.value(iter.Value(), depth+1); err != nil {
				return err
			}
		}
	case reflect.Struct:
		if err := b.add(4); err != nil {
			return err
		}
		for i := 0; i < v.NumField(); i++ {
			field := v.Type().Field(i)
			if field.PkgPath != "" || field.Tag.Get("json") == "-" {
				continue
			}
			if err := b.str(field.Name); err != nil {
				return err
			}
			if err := b.value(v.Field(i), depth+1); err != nil {
				return err
			}
		}
	default:
		return ErrCapacity
	}
	return nil
}
