package servingauth

import (
	"errors"

	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// passwordRefusal is issued only before authenticateInCommand stages any
// credential or factor effects. It carries no successful authentication proof.
type passwordRefusal struct{ tx *sqlite.CommandTx }

func (*passwordRefusal) Error() string { return ErrCredentials.Error() }
func (*passwordRefusal) Unwrap() error { return ErrCredentials }

func refusePassword(tx *sqlite.CommandTx, err error) error {
	if errors.Is(err, ErrCredentials) {
		return &passwordRefusal{tx: tx}
	}
	return err
}

// IsReadOnlyPasswordRefusal recognizes only this command's package-issued
// pre-mutation refusal. An arbitrary credential error, later final check or
// enrollment failure must still roll back the whole command. This permits a
// dedicated login command to commit only its attempted-password counters.
func IsReadOnlyPasswordRefusal(err error, tx *sqlite.CommandTx) bool {
	var refusal *passwordRefusal
	return tx != nil && errors.As(err, &refusal) && refusal.tx == tx
}
