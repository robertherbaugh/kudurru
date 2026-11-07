package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/robertherbaugh/kudurru/internal/iamlegacy"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

func migratePublicIAM(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT kind,namespace,name,uid,data FROM objects WHERE kind IN ('ServiceAccount','RoleBinding','IdentityProvider') ORDER BY kind,namespace,name`)
	if err != nil {
		return err
	}
	type row struct {
		kind, namespace, name, uid string
		raw                        []byte
	}
	var documents []row
	bytes := 0
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.kind, &r.namespace, &r.name, &r.uid, &r.raw); err != nil {
			_ = rows.Close()
			return err
		}
		bytes += len(r.raw)
		if bytes > 64<<20 || len(r.raw) > iamlegacy.MaxDocumentBytes || len(documents) >= 100000 {
			_ = rows.Close()
			return errors.New("legacy IAM migration exceeds bounded document budget")
		}
		documents = append(documents, r)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	if len(documents) == 0 {
		return nil
	}
	v, err := validator.New()
	if err != nil {
		return errors.New("public IAM admission schema unavailable")
	}
	for _, r := range documents {
		obj, err := iamlegacy.Convert(r.kind, r.raw)
		if err != nil || obj.GetName() != r.name || obj.GetNamespace() != r.namespace || obj.GetUID() == "" || string(obj.GetUID()) != r.uid {
			return errors.New("invalid stored IAM identity")
		}
		// Validate a copy as a new object so ratcheting cannot hide a now-invalid
		// field; persist the original converted identity/status, not admission's
		// create projection, which intentionally clears server metadata/status.
		if err := v.Validate(ctx, obj.DeepCopyObject().(store.Object), nil); err != nil {
			return errors.New("stored IAM configuration fails public admission")
		}
		raw, err := json.Marshal(obj)
		if err != nil {
			return errors.New("encode migrated IAM identity")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE objects SET data=?,spec_hash=? WHERE kind=? AND namespace=? AND name=?`, string(raw), specHash(obj), r.kind, r.namespace, r.name); err != nil {
			return err
		}
	}
	return nil
}
