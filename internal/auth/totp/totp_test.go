package totp

import (
	"crypto/sha1" //nolint:gosec // RFC6238 official HMAC-SHA1 vectors.
	"crypto/sha256"
	"crypto/sha512"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func TestRFC6238OfficialVectors(t *testing.T) {
	vectors := []struct {
		unix                 uint64
		sha1, sha256, sha512 string
	}{
		{59, "94287082", "46119246", "90693936"},
		{1111111109, "07081804", "68084774", "25091201"},
		{1111111111, "14050471", "67062674", "99943326"},
		{1234567890, "89005924", "91819424", "93441116"},
		{2000000000, "69279037", "90698825", "38618901"},
		{20000000000, "65353130", "77737706", "47863826"},
	}
	keys := [][]byte{[]byte("12345678901234567890"), []byte("12345678901234567890123456789012"), []byte("1234567890123456789012345678901234567890123456789012345678901234")}
	algorithms := []func() hash.Hash{sha1.New, sha256.New, sha512.New}
	for _, v := range vectors {
		for i, want := range []string{v.sha1, v.sha256, v.sha512} {
			got, err := rfcCode(keys[i], v.unix/30, 8, algorithms[i])
			if err != nil || got != want {
				t.Fatal("RFC vector mismatch at timestamp/algorithm", v.unix, i)
			}
		}
	}
}
func testSeed(t *testing.T) *Seed {
	t.Helper()
	s, err := Generate(Account{Name: "alice", UID: "exact-uid"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	copy(s.key[:], []byte("12345678901234567890"))
	return s
}
func TestFixedProfileBoundsWindowAndReplay(t *testing.T) {
	s := testSeed(t)
	now := time.Unix(1111111109, 0)
	code, _ := rfcCode(s.key[:], uint64(37037036), 6, sha1.New)
	if !strings.HasPrefix(code, "0") {
		t.Fatal("vector must exercise leading zero")
	}
	step, err := Match(s, code, now)
	if err != nil || step != uint64(37037036) {
		t.Fatal("profile mismatch", err)
	}
	for _, at := range []time.Time{now.Add(-30 * time.Second), now.Add(30 * time.Second)} {
		if _, err := Match(s, code, at); err != nil {
			t.Fatal("bounded adjacent window denied", err)
		}
	}
	for _, bad := range []string{code + "0", code[1:], " " + code, "00000x", strings.Repeat("0", 10000)} {
		if _, err := Match(s, bad, now); !errors.Is(err, ErrProof) {
			t.Fatal("malformed code accepted", err)
		}
	}
	if _, err := Match(s, code, now.Add(60*time.Second)); !errors.Is(err, ErrProof) {
		t.Fatal("unbounded skew accepted", err)
	}
	if _, err := Match(s, code, time.Unix(-1, 0).In(time.FixedZone("positive", 3600))); !errors.Is(err, ErrProof) {
		t.Fatal("negative epoch accepted", err)
	}
	if _, err := Match(s, code, time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)); !errors.Is(err, ErrProof) {
		t.Fatal("time cap bypassed", err)
	}
	if _, err := rfcCode(make([]byte, 65), 0, 6, sha1.New); !errors.Is(err, ErrInvalid) {
		t.Fatal("seed cap bypassed", err)
	}
	if _, err := rfcCode(s.key[:], math.MaxUint64, 7, sha1.New); !errors.Is(err, ErrInvalid) {
		t.Fatal("digit profile widened", err)
	}
	a, version, id := s.Identity()
	r := Record{FormatVersion: 1, Account: a, SeedVersion: version, SeedID: id, Enabled: true}
	zero, err := Advance(r, s, 0)
	if err != nil || !zero.HasConsumed || zero.LastConsumedStep != 0 {
		t.Fatal("zero counter confused with unused", err)
	}
	if _, err = Advance(zero, s, 0); !errors.Is(err, ErrReplay) {
		t.Fatal("zero replay accepted", err)
	}
	r, err = Advance(r, s, step)
	if err != nil {
		t.Fatal(err)
	}
	for _, old := range []uint64{step, step - 1} {
		if _, err = Advance(r, s, old); !errors.Is(err, ErrReplay) {
			t.Fatal("floor lowered", err)
		}
	}
	changed := *s
	changed.version++
	if _, err = Advance(r, &changed, step+1); !errors.Is(err, ErrProof) {
		t.Fatal("seed version mismatch accepted", err)
	}
	r.Enabled = false
	if _, err = Advance(r, s, step+1); !errors.Is(err, ErrProof) {
		t.Fatal("disabled factor accepted", err)
	}
}
func TestSealedUIDPurposeRotationAndStrictPayload(t *testing.T) {
	s := testSeed(t)
	current := []byte("01234567890123456789012345678901")
	k, err := NewKeyring(current, nil)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := k.Seal(s)
	if err != nil {
		t.Fatal(err)
	}
	current[0] = 'x'
	opened, err := k.Open(s.account, sealed)
	if err != nil || opened.key != s.key {
		t.Fatal("key/seed alias", err)
	}
	if _, err = k.Open(Account{Name: "alice", UID: "new-uid"}, sealed); err == nil {
		t.Fatal("recreated UID accepted seed")
	}
	if _, err = k.Open(Account{Name: "bob", UID: s.account.UID}, sealed); err == nil {
		t.Fatal("another account adopted seed")
	}
	rotated, err := NewKeyring([]byte("11234567890123456789012345678901"), []byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	opened, err = rotated.Open(s.account, sealed)
	if err != nil || opened.id != s.id || opened.version != s.version {
		t.Fatal("key rotation reset seed identity", err)
	}
	resealed, err := rotated.Seal(opened)
	if err != nil {
		t.Fatal(err)
	}
	newOnly, _ := NewKeyring([]byte("11234567890123456789012345678901"), nil)
	if _, err = newOnly.Open(s.account, resealed); err != nil {
		t.Fatal(err)
	}
	if _, err = newOnly.Open(s.account, sealed); err == nil {
		t.Fatal("retired key accepted")
	}
	if k.Identity() == rotated.Identity() {
		t.Fatal("resource cursor did not change")
	}
	other, _ := session.NewSealer(session.PurposeSession, []byte("01234567890123456789012345678901"), nil)
	raw, _, err := k.sealer.Open(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = k.Open(s.account, other.Seal(raw)); err == nil {
		t.Fatal("session purpose adopted as seed")
	}
	for _, what := range []string{"version", "policy", "counter", "base32", "duplicate", "case-alias", "missing", "oversized"} {
		t.Run(what, func(t *testing.T) {
			var doc map[string]json.RawMessage
			_ = json.Unmarshal(raw, &doc)
			switch what {
			case "version":
				doc["formatVersion"] = []byte(`99`)
			case "policy":
				doc["period"] = []byte(`60`)
			case "counter":
				doc["seedVersion"] = []byte(`"01"`)
			case "base32":
				doc["secret"], _ = json.Marshal(strings.ToLower(s.ProvisioningSecret()))
			case "missing":
				delete(doc, "digits")
			case "oversized":
				doc["secret"], _ = json.Marshal(strings.Repeat("A", MaxSeedBytes+1))
			}
			data, _ := json.Marshal(doc)
			if what == "duplicate" {
				data = append([]byte(`{"digits":6,`), data[1:]...)
			}
			if what == "case-alias" {
				data = append([]byte(`{"Digits":6,`), data[1:]...)
			}
			if _, err := k.Open(s.account, k.sealer.Seal(data)); err == nil {
				t.Fatal("unsupported seed payload accepted")
			}
		})
	}
	for _, v := range []any{s, k} {
		formatted := fmt.Sprintf("%#v", v)
		jsonData, _ := json.Marshal(v)
		if strings.Contains(formatted, s.ProvisioningSecret()) || strings.Contains(string(jsonData), s.ProvisioningSecret()) {
			t.Fatal("seed escaped diagnostics")
		}
	}
}
func TestRecordStrictRequiredBooleanCapsAndDetached(t *testing.T) {
	s := testSeed(t)
	a, version, id := s.Identity()
	r := Record{FormatVersion: 1, Account: a, SeedVersion: version, SeedID: id, Enabled: true}
	row, err := EncodeRecord(r)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeRecord(row)
	if err != nil || decoded != r {
		t.Fatal("record roundtrip", err)
	}
	for _, what := range []string{"enabled", "hasConsumed", "version", "counter", "duplicate", "case-alias", "null", "unconsumed-floor", "key", "huge"} {
		t.Run(what, func(t *testing.T) {
			entry := replication.Entry{Key: row.Key, Data: slices.Clone(row.Data)}
			var doc map[string]json.RawMessage
			_ = json.Unmarshal(entry.Data, &doc)
			switch what {
			case "enabled", "hasConsumed":
				delete(doc, what)
			case "version":
				doc["formatVersion"] = []byte(`2`)
			case "counter":
				doc["lastConsumedStep"] = []byte(`"00"`)
			case "null":
				doc["enabled"] = []byte(`null`)
			case "unconsumed-floor":
				doc["lastConsumedStep"] = []byte(`"1"`)
			case "key":
				entry.Key += "wrong"
			}
			entry.Data, _ = json.Marshal(doc)
			if what == "duplicate" {
				entry.Data = append([]byte(`{"enabled":true,`), entry.Data[1:]...)
			}
			if what == "case-alias" {
				entry.Data = append([]byte(`{"Enabled":true,`), entry.Data[1:]...)
			}
			if what == "huge" {
				entry.Data = []byte(strings.Repeat(" ", MaxRecordBytes+1))
			}
			if _, err := DecodeRecord(entry); err == nil {
				t.Fatal("malformed replay state accepted")
			}
		})
	}
	clear(row.Data)
	if decoded.Account.UID != a.UID {
		t.Fatal("record aliases raw document")
	}
}

func TestWindowCollisionChoosesLargestStepAndNeverFallsBack(t *testing.T) {
	s := testSeed(t)
	// Public RFC key has a six-digit collision at these two nearby steps.
	code, _ := rfcCode(s.key[:], 153567, 6, sha1.New)
	next, _ := rfcCode(s.key[:], 153569, 6, sha1.New)
	if code != next {
		t.Fatal("collision fixture changed")
	}
	step, err := Match(s, code, time.Unix(153568*30, 0))
	if err != nil || step != 153569 {
		t.Fatal("collision selected a lower step", err)
	}
	a, version, id := s.Identity()
	r := Record{FormatVersion: 1, Account: a, SeedVersion: version, SeedID: id, Enabled: true, HasConsumed: true, LastConsumedStep: step}
	if _, err = Advance(r, s, step); !errors.Is(err, ErrReplay) {
		t.Fatal("collision replay found an older candidate", err)
	}
}
