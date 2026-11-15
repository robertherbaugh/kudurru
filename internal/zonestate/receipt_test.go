package zonestate

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
)

func testReceipt(id string, notAfter int64) Receipt {
	return Receipt{ID: id, SemanticDigest: strings.Repeat("a", 64), Principal: dynamicupdate.Principal{Key: "key.", Version: "v1"}, AuthEpoch: 1, CommitSequence: 2, Serial: 11, NotAfter: notAfter}
}

// A receipt is live until ReceiptClockMargin after its signature deadline,
// a fractional deadline rounds up, and a receipt without a deadline (written
// before D409) is not live.
func TestReceiptLiveUntilDeadlinePlusMargin(t *testing.T) {
	deadline := testNow()
	if got := ReceiptNotAfter(deadline.Add(time.Millisecond)); got != deadline.Unix()+1 {
		t.Fatal("fractional deadline rounded down", got)
	}
	if got := ReceiptNotAfter(deadline); got != deadline.Unix() {
		t.Fatal("whole deadline moved", got)
	}
	r := testReceipt("r", deadline.Unix())
	end := deadline.Add(ReceiptClockMargin)
	if !r.Live(deadline) || !r.Live(end.Add(-time.Nanosecond)) || r.Live(end) {
		t.Fatal("live window is not [deadline + margin)")
	}
	if testReceipt("legacy", 0).Live(time.Unix(0, 0)) {
		t.Fatal("a receipt without a deadline is live")
	}
	kept := LiveReceipts([]Receipt{testReceipt("a", deadline.Unix()), testReceipt("b", deadline.Unix()+3600), testReceipt("c", 0)}, end)
	if len(kept) != 1 || kept[0].ID != "b" {
		t.Fatal("live receipts", kept)
	}
}

// The deadline is part of the committed format: it round-trips through
// Prepare and Decode, an aggregate without it (pre-release v0.2 data) still
// decodes, and a negative one is refused.
func TestReceiptDeadlineFormat(t *testing.T) {
	a := fixture(t)
	a.Receipts = []Receipt{testReceipt("with", testNow().Unix()), testReceipt("without", 0)}
	p, err := Prepare(a, nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := readPrepared(p, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Receipts) != 2 || got.Receipts[0] != a.Receipts[0] || got.Receipts[1] != a.Receipts[1] {
		t.Fatal("receipts did not round-trip", got.Receipts)
	}
	a.Receipts = []Receipt{testReceipt("negative", -1)}
	if _, err := Prepare(a, nil, Limits{}); !errors.Is(err, ErrInvalid) {
		t.Fatal("negative deadline accepted", err)
	}
}
