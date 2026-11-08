package security

import (
	"context"
	"errors"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/loginlimit"
	"github.com/robertherbaugh/kudurru/internal/auth/loginlimit/sqliteadapter"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// IssueLocalWithAdmission stages counters and enrollment in the SAME original
// command. Refused is an authentication response, not a command error: only an
// exact pre-mutation password/factor refusal may commit failure counters. Every
// other error aborts the command. A nil command error grants neither a cookie nor
// success until the caller commits and observes the enrollment as usual.
// Call this before any other business mutation in the dedicated login command.
func IssueLocalWithAdmission(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies, proof servingauth.Credential, ip string, clock func() time.Time) (enrollment Enrollment, refused error, commandErr error) {
	if clock == nil {
		clock = time.Now
	}
	proof.Username = users.NormalizeUsername(proof.Username)
	limit, err := sqliteadapter.New(ctx, tx, clock)
	if err != nil {
		return enrollment, nil, err
	}
	ticket, err := limit.Begin(ctx, ip, proof.Username)
	var locked *loginlimit.Locked
	if errors.As(err, &locked) {
		return enrollment, locked, nil
	}
	if err != nil {
		return enrollment, nil, err
	}
	enrollment, err = IssueLocal(ctx, tx, deps, proof, clock().UTC())
	if err != nil {
		if !servingauth.IsReadOnlyPasswordRefusal(err, tx) {
			return Enrollment{}, nil, err
		}
		if finishErr := ticket.Finish(ctx, false); finishErr != nil {
			return Enrollment{}, nil, finishErr
		}
		return Enrollment{}, err, nil
	}
	if err := ticket.Finish(ctx, true); err != nil {
		return Enrollment{}, nil, err
	}
	return enrollment, nil, nil
}
