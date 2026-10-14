package main

import (
	"bytes"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/audit/auditlog"
)

func TestAuditSealKeysRotation(t *testing.T) {
	current, previous := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	keys, err := auditSealKeys(current, previous)
	if err != nil {
		t.Fatal(err)
	}
	wantCurrent, _ := auditlog.SealKey(current)
	wantPrevious, _ := auditlog.SealKey(previous)
	if len(keys) != 2 || !bytes.Equal(keys[0], wantCurrent) || !bytes.Equal(keys[1], wantPrevious) {
		t.Fatal("writers must use the current seal key and readers must accept the previous one")
	}
	keys, err = auditSealKeys(current, nil)
	if err != nil || len(keys) != 1 || !bytes.Equal(keys[0], wantCurrent) {
		t.Fatalf("without previous key: %v", err)
	}
}
