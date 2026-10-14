// Package servingdns adapts an owner-neutral serving publisher to DNS request
// leases. Root composition supplies typed owner components and stage callbacks.
package servingdns

import (
	"fmt"
	"reflect"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/serving"
)

type lease struct{ inner *serving.Lease }

// Wrap transfers ownership of one concrete lease without cloning it.
func Wrap(inner *serving.Lease) pipeline.ServingLease {
	if inner == nil {
		return nil
	}
	return &lease{inner: inner}
}

func (l *lease) Value() any { return l.inner.Bundle() }

func (l *lease) Clone() (pipeline.ServingLease, error) {
	inner, err := l.inner.Clone()
	return Wrap(inner), err
}

func (l *lease) Release() { l.inner.Release() }

// Capture returns the single Chain hook. It never creates substitute components
// or uses the store's installed cursor as evidence of a served generation.
func Capture(p *serving.Publisher) pipeline.Capture {
	return func(*pipeline.Request) (pipeline.ServingLease, error) {
		if p == nil {
			return nil, pipeline.ErrServingCapture
		}
		l, err := p.Capture()
		return Wrap(l), err
	}
}

// Component reads an immutable typed owner value while the request holds its
// lease. A truthful owner wrapper may contain known-empty data; missing, wrongly
// typed or typed-nil component values fail closed.
func Component[T any](req *pipeline.Request, name string) (T, error) {
	var zero T
	if req == nil || req.Serving == nil {
		return zero, pipeline.ErrServingCapture
	}
	b, ok := req.Serving.Value().(*serving.Bundle)
	if !ok || b == nil {
		return zero, pipeline.ErrServingCapture
	}
	v, found := b.Component(name)
	typed, ok := v.(T)
	if !found || !ok || isNil(typed) {
		return zero, fmt.Errorf("%w: component %q", pipeline.ErrServingCapture, name)
	}
	return typed, nil
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}
