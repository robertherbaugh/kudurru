package host

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/robertherbaugh/kudurru/ee/integration"
)

type fakeActions struct {
	fakePublisher
	calls  int
	result json.RawMessage
	params json.RawMessage
}

func (p *fakeActions) Actions() []integration.ActionSpec {
	return []integration.ActionSpec{{Name: "block", Params: p.params, Verb: "integrations.act"}}
}
func (p *fakeActions) Invoke(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	p.calls++
	return p.result, nil
}
func TestAdvertisedActionSchemaAndEscapedSecretRedaction(t *testing.T) {
	h, j, _, _, _ := engineFixture(t)
	p := &fakeActions{params: json.RawMessage(`{"type":"object","properties":{"mac":{"type":"string","pattern":"^[a-f0-9:]{17}$"}},"required":["mac"],"additionalProperties":false}`), result: json.RawMessage(`{"success":true}`)}
	j.provider = p
	for _, params := range []string{`{}`, `{"mac":"bad"}`, `{"mac":"00:11:22:33:44:55","other":true}`} {
		a := h.execute(context.Background(), j, command{kind: "invoke", action: "block", params: json.RawMessage(params)})
		if !errors.Is(a.err, ErrInvalid) || p.calls != 0 {
			t.Fatal("invalid action invoked", a.err)
		}
	}
	a := h.execute(context.Background(), j, command{kind: "invoke", action: "foreign", params: json.RawMessage(`{}`)})
	if !errors.Is(a.err, ErrUnsupported) || p.calls != 0 {
		t.Fatal("unadvertised action invoked", a.err)
	}
	j.frame.instance.secrets["token"] = []byte("secret-value")
	for _, result := range []string{`{"nested":{"token":"s\u0065cret-value"}}`, `{"token":"s\u0065cret-value","token":"redacted"}`} {
		p.result = json.RawMessage(result)
		a := h.execute(context.Background(), j, command{kind: "invoke", action: "block", params: json.RawMessage(`{"mac":"00:11:22:33:44:55"}`)})
		if !errors.Is(a.err, ErrProvider) {
			t.Fatal("escaped secret returned", a.err)
		}
	}
	p.result = json.RawMessage(`{"success":true}`)
	a = h.execute(context.Background(), j, command{kind: "invoke", action: "block", params: json.RawMessage(`{"mac":"00:11:22:33:44:55"}`)})
	if a.err != nil || string(a.value) != string(p.result) {
		t.Fatal(a.err)
	}
	p.params = json.RawMessage(`{"$ref":"https://schema.example/remote"}`)
	a = h.execute(context.Background(), j, command{kind: "invoke", action: "block", params: json.RawMessage(`{}`)})
	if !errors.Is(a.err, ErrUnsupported) {
		t.Fatal("remote schema fetched/ignored", a.err)
	}
}
