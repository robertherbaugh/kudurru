package sqlite

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment/accepted"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func TestReadMemberEnrollmentPendingIsNonGranting(t *testing.T) {
	for _, enroll := range []bool{false, true} {
		t.Run(map[bool]string{false: "unenrolled", true: "awaiting-first-image"}[enroll], func(t *testing.T) {
			s := openTemp(t, func(o *Options) { o.InitialMode = replication.Follower })
			if enroll {
				if err := s.Enroll(t.Context(), replication.Term{ClusterID: "pending-cluster", Epoch: 1, PrimaryID: "primary", PromotionID: "initial"}); err != nil {
					t.Fatal(err)
				}
			}
			before := anchorDBRows(t, s.r, false)
			a, owner, err := s.ReadMemberEnrollment(t.Context())
			if !errors.Is(err, ErrMemberEnrollmentPending) || owner.Digest() != "" || owner.State() != nil || !reflect.DeepEqual(a, s.Authority()) {
				t.Fatal("pending receiver gained a complete member owner", err)
			}
			if _, err := s.ReadGeneration(t.Context()); !errors.Is(err, replication.ErrReadOnly) {
				t.Fatal("pending observation created an image", err)
			}
			if !reflect.DeepEqual(before, anchorDBRows(t, s.r, false)) {
				t.Fatal("pending observation changed stored rows")
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if _, _, err := s.ReadMemberEnrollment(ctx); !errors.Is(err, context.Canceled) {
				t.Fatal("pending classification hid cancellation", err)
			}
		})
	}
}

func TestReadMemberEnrollmentRefusesCorruptPendingOwner(t *testing.T) {
	for _, mode := range []string{"missing", "unknown", "extra-field", "orphan", "installed", "prior-install", "standalone"} {
		t.Run(mode, func(t *testing.T) {
			s := openTemp(t, func(o *Options) {
				if mode != "standalone" {
					o.InitialMode = replication.Follower
				}
			})
			if mode != "standalone" {
				if err := s.Enroll(t.Context(), replication.Term{ClusterID: "pending-cluster", Epoch: 1, PrimaryID: "primary", PromotionID: "initial"}); err != nil {
					t.Fatal(err)
				}
			}
			// Deliberate corruption tests the read boundary; normal writes and
			// installation cannot introduce these incomplete histories.
			var err error
			switch mode {
			case "missing":
				_, err = s.w.ExecContext(t.Context(), `DELETE FROM replication_sections WHERE section=? AND key=?`, accepted.Section, accepted.RootKey)
			case "unknown", "extra-field":
				raw := `{"phase":"awaiting","version":2}`
				if mode == "extra-field" {
					raw = `{"phase":"awaiting","version":1,"extra":true}`
				}
				_, err = s.w.ExecContext(t.Context(), `UPDATE replication_sections SET data=? WHERE section=? AND key=?`, raw, accepted.Section, accepted.RootKey)
			case "orphan":
				_, err = s.w.ExecContext(t.Context(), `INSERT INTO replication_sections(section,key,data) VALUES(?,?,?)`, accepted.Section, accepted.Prefix+"orphan", `{}`)
			case "installed", "prior-install":
				a := s.Authority()
				if mode == "installed" {
					a.Installed = replication.Generation{Term: a.Term}
				} else {
					a.InstallSequence = 1
				}
				tx, beginErr := s.w.BeginTx(t.Context(), nil)
				if beginErr != nil {
					t.Fatal(beginErr)
				}
				defer func() { _ = tx.Rollback() }()
				err = writeAuthority(t.Context(), tx, a)
				if err == nil {
					err = tx.Commit()
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			before := anchorDBRows(t, s.r, false)
			_, owner, err := s.ReadMemberEnrollment(t.Context())
			if !errors.Is(err, accepted.ErrOwner) || errors.Is(err, ErrMemberEnrollmentPending) || owner.Digest() != "" {
				t.Fatal("corrupt or initialized owner treated as pending", err)
			}
			if !reflect.DeepEqual(before, anchorDBRows(t, s.r, false)) {
				t.Fatal("refused observation repaired stored rows")
			}
		})
	}
}
