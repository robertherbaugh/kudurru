package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/store"
)

func TestForwardedObjectCommandRepeatsNormalizationAndRefusesMismatch(t *testing.T) {
	h := newHarness(t)
	doc := func(v map[string]any) json.RawMessage {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	record := map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSRecordSet",
		"metadata": map[string]any{"name": "r", "namespace": "default"},
		"spec":     map[string]any{"zoneRef": map[string]any{"name": "z"}, "endpoints": []any{map[string]any{"dnsName": "a.z.test.", "recordType": "A", "targets": []any{"192.0.2.1"}}}}}
	key, run, err := h.srv.ForwardedObjectCommand(ForwardedObject{Operation: ForwardedWrite, Kind: "DNSRecordSet", Namespace: "default", Name: "r", Document: doc(record)})
	if err != nil || run == nil || key != (store.Key{Kind: "DNSRecordSet", Namespace: "default", Name: "r"}) {
		t.Fatal("valid forwarded write refused", err)
	}
	for name, in := range map[string]ForwardedObject{
		"unsupported kind":     {Operation: ForwardedWrite, Kind: "Secret", Namespace: "default", Name: "r", Document: doc(record)},
		"cluster namespace":    {Operation: ForwardedWrite, Kind: "DNSZone", Namespace: "default", Name: "z", Document: doc(record)},
		"missing namespace":    {Operation: ForwardedWrite, Kind: "DNSRecordSet", Name: "r", Document: doc(record)},
		"document kind":        {Operation: ForwardedWrite, Kind: "BlockList", Namespace: "default", Name: "r", Document: doc(record)},
		"document name":        {Operation: ForwardedWrite, Kind: "DNSRecordSet", Namespace: "default", Name: "other", Document: doc(record)},
		"trailing document":    {Operation: ForwardedWrite, Kind: "DNSRecordSet", Namespace: "default", Name: "r", Document: append(doc(record), []byte(" {}")...)},
		"empty document":       {Operation: ForwardedWrite, Kind: "DNSRecordSet", Namespace: "default", Name: "r"},
		"delete with document": {Operation: ForwardedDelete, Kind: "DNSRecordSet", Namespace: "default", Name: "r", Document: doc(record)},
		"delete with manager":  {Operation: ForwardedDelete, Kind: "DNSRecordSet", Namespace: "default", Name: "r", Manager: "api"},
		"unknown operation":    {Operation: "patch", Kind: "DNSRecordSet", Namespace: "default", Name: "r", Document: doc(record)},
	} {
		if _, run, err := h.srv.ForwardedObjectCommand(in); err == nil || run != nil {
			t.Fatal("forwarded mutation not refused:", name)
		} else if refusal, ok := ForwardedObjectRefusal(err); !ok || refusal.Status != http.StatusBadRequest {
			t.Fatal("refusal is not the public invalid error:", name, err)
		}
	}
}

func TestForwardedObjectRefusalClassification(t *testing.T) {
	for _, tc := range []struct {
		err   error
		relay bool
	}{
		{errPrecondition("1", "2"), true},
		{errConflict("exists", ""), true},
		{store.ErrConflict, true},
		{store.ErrNotFound, true},
		{newError(http.StatusForbidden, KeyForbidden, "denied", ""), false},
		{newError(http.StatusUnauthorized, KeyUnauthenticated, "who", ""), false},
		{errUnavailable("busy"), false},
		{context.Canceled, false},
		{errors.New("driver failure"), false},
	} {
		if _, ok := ForwardedObjectRefusal(tc.err); ok != tc.relay {
			t.Fatal("refusal classification", tc.err, ok)
		}
	}
}

func TestForwardedWriteResultRejectsInvalidPrimaryOutput(t *testing.T) {
	k, _ := kindByPlural("dnsrecordsets")
	key := store.Key{Kind: "DNSRecordSet", Namespace: "default", Name: "r"}
	good := json.RawMessage(`{"apiVersion":"kudurru.io/v1alpha1","kind":"DNSRecordSet","metadata":{"name":"r","namespace":"default"}}`)
	if _, err := forwardedWriteResult(k, key, ForwardedObjectResult{Result: ResultCreated, Object: good}); err != nil {
		t.Fatal(err)
	}
	for _, out := range []ForwardedObjectResult{
		{Result: "deleted", Object: good},
		{Result: ResultCreated, Object: json.RawMessage(`{"apiVersion":"kudurru.io/v1alpha1","kind":"DNSRecordSet","metadata":{"name":"other","namespace":"default"}}`)},
		{Result: ResultCreated, Object: json.RawMessage(`{"unknown":true}`)},
	} {
		if _, err := forwardedWriteResult(k, key, out); err == nil {
			t.Fatal("invalid primary output accepted", out.Result)
		}
	}
}
