package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// migrateCredentialUIDs runs before normal decoding or watch publication. It
// preserves hashes, metadata, ownership, status and audit history. An orphaned
// or ambiguous credential remains unbound and therefore cannot authenticate.
func migrateCredentialUIDs(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT data FROM objects WHERE kind = 'APIToken' ORDER BY namespace, name`)
	if err != nil {
		return err
	}
	var tokens []*internalv1.APIToken
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			_ = rows.Close()
			return err
		}
		var token internalv1.APIToken
		if err := json.Unmarshal(raw, &token); err != nil {
			_ = rows.Close()
			return errors.New("invalid stored API token document")
		}
		if err := token.Validate(); err != nil {
			_ = rows.Close()
			return errors.New("invalid stored API token")
		}
		tokens = append(tokens, &token)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, token := range tokens {
		owner := &token.Spec.Owner
		if owner.UID != "" {
			continue
		}
		var account store.Object
		switch {
		case owner.Kind == internalv1.OwnerServiceAccount:
			account = &internalv1.ServiceAccount{}
		case owner.Kind == internalv1.OwnerUser && (owner.Provider == "local" || strings.HasPrefix(owner.ID, "local:")):
			if owner.ID != "local:"+owner.Name || owner.Namespace != "" {
				continue
			}
			account = &internalv1.LocalUser{}
		default:
			continue
		}
		kind := store.KindOf(account)
		var raw []byte
		err := tx.QueryRowContext(ctx, `SELECT data FROM objects WHERE kind = ? AND namespace = ? AND name = ?`,
			kind, owner.Namespace, owner.Name).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if json.Unmarshal(raw, account) != nil || account.GetUID() == "" || account.GetName() != owner.Name || account.GetNamespace() != owner.Namespace {
			return errors.New("invalid stored token owner identity")
		}
		created := account.GetCreationTimestamp()
		if created.IsZero() || token.CreationTimestamp.IsZero() || token.CreationTimestamp.Before(&created) {
			continue
		}
		if token.CreationTimestamp.Equal(&created) {
			// Timestamps have second resolution. Require the durable creation
			// order for ties, rather than guessing across a name replacement.
			accountRV, err := creationRV(ctx, tx, kind, owner.Namespace, owner.Name)
			if err != nil {
				return err
			}
			tokenRV, err := creationRV(ctx, tx, internalv1.KindAPIToken, token.Namespace, token.Name)
			if err != nil {
				return err
			}
			if accountRV == 0 || tokenRV == 0 || tokenRV <= accountRV {
				continue
			}
		}
		owner.UID = string(account.GetUID())
		data, err := json.Marshal(token)
		if err != nil {
			return errors.New("encode migrated token")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE objects SET data = ?, spec_hash = ? WHERE kind = 'APIToken' AND namespace = ? AND name = ?`,
			string(data), specHash(token), token.Namespace, token.Name); err != nil {
			return fmt.Errorf("persist credential identity: %w", err)
		}
	}
	return nil
}

func creationRV(ctx context.Context, tx *sql.Tx, kind store.Kind, namespace, name string) (uint64, error) {
	var rv uint64
	err := tx.QueryRowContext(ctx, `SELECT resource_version FROM audit WHERE kind = ? AND namespace = ? AND name = ? AND action = ? AND result = 'success' ORDER BY id DESC LIMIT 1`,
		kind, namespace, name, strings.ToLower(string(kind))+".create").Scan(&rv)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return rv, err
}
