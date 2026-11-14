package parser

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestSlowStreamIndependentOfDiagnosticLimitAndBufferLifetime(t *testing.T) {
	var body strings.Builder
	for i := 0; i < 3000; i++ {
		fmt.Fprintf(&body, "||host%d.example^$dnstype=AAAA,client='Fixture PC'\n", i)
	}
	for _, limit := range []int{-1, 1, 1000} {
		var retained []UnsupportedRule
		res, err := Parse(strings.NewReader(body.String()), Options{Format: FormatAdblock, KeepUnsupported: limit, EmitSlow: func(u UnsupportedRule) error {
			u.Text, u.Pattern = strings.Clone(u.Text), strings.Clone(u.Pattern)
			u.Modifiers = append([]Modifier(nil), u.Modifiers...)
			for i := range u.Modifiers {
				u.Modifiers[i].Name = strings.Clone(u.Modifiers[i].Name)
				u.Modifiers[i].Value = strings.Clone(u.Modifiers[i].Value)
			}
			retained = append(retained, u)
			return ErrUnsupportedSlow
		}}, nil)
		if err != nil || res.Unsupported != 3000 || len(retained) != 3000 || len(res.UnsupportedRules) != max(limit, 0) {
			t.Fatalf("limit %d: result=%+v retained=%d err=%v", limit, res, len(retained), err)
		}
		for i, u := range retained {
			if u.Pattern != fmt.Sprintf("||host%d.example^", i) || u.Modifiers[1].Value != "'Fixture PC'" {
				t.Fatalf("read buffer corrupted retained candidate %d: %+v", i, u)
			}
		}
	}
}

func TestSlowStreamAcceptanceInvalidAndFatalErrors(t *testing.T) {
	res, err := Parse(strings.NewReader("@@||allowed.example^$important\n||invalid.example^$dnstype=NONE\n||unknown.example^$unknown"), Options{Format: FormatAdblock, EmitSlow: func(u UnsupportedRule) error {
		if u.Exception {
			return nil
		}
		return fmt.Errorf("%w: unknown RR type", ErrInvalidSlow)
	}}, nil)
	if err != nil || res.Rules != 1 || res.Exceptions != 1 || res.Invalid != 2 || res.InvalidReasons["invalid-modifier"] != 1 || res.InvalidReasons[ReasonUnknownModifier] != 1 || res.Unsupported != 0 {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	stop := errors.New("compiler stopped")
	calls := 0
	_, err = Parse(strings.NewReader("||one.example^$important\n||two.example^$important"), Options{EmitSlow: func(UnsupportedRule) error { calls++; return stop }}, nil)
	if !errors.Is(err, stop) || calls != 1 {
		t.Fatalf("fatal error not propagated: %v, calls %d", err, calls)
	}
}

func TestModifierTrailingEscapeRejectsWholeRule(t *testing.T) {
	for _, text := range []string{"client=Fixture\\", "dnstype=A\\", "dnsrewrite=192.0.2.1\\"} {
		if _, err := ParseModifiers(text); !errors.Is(err, ErrMalformedModifiers) {
			t.Fatalf("trailing escape accepted: %q %v", text, err)
		}
		calls := 0
		res, err := Parse(strings.NewReader("||escape.example^$"+text), Options{EmitSlow: func(UnsupportedRule) error { calls++; return nil }}, nil)
		if err != nil || res.Invalid != 1 || res.Rules != 0 || calls != 0 {
			t.Fatalf("malformed condition broadened: %+v %v calls=%d", res, err, calls)
		}
	}
}
