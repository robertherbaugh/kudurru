package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func migratePrincipalStates(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT data FROM objects WHERE kind='ServiceAccount' ORDER BY namespace, name`)
	if err != nil {
		return err
	}
	var documents [][]byte
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			_ = rows.Close()
			return err
		}
		documents = append(documents, raw)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, raw := range documents {
		var legacy struct {
			Spec struct {
				BuiltinAdmin bool `json:"builtinAdmin"`
			} `json:"spec"`
		}
		var sa internalv1.ServiceAccount
		if json.Unmarshal(raw, &legacy) != nil || json.Unmarshal(raw, &sa) != nil || sa.Validate() != nil || sa.UID == "" {
			return errors.New("invalid stored service account identity")
		}
		if legacy.Spec.BuiltinAdmin {
			owner := internalv1.TokenOwner{Kind: internalv1.OwnerServiceAccount, Name: sa.Name, Namespace: sa.Namespace, UID: string(sa.UID), ID: "sa:" + sa.Namespace + "/" + sa.Name, Provider: "kudurru"}
			state := &internalv1.PrincipalState{
				TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: internalv1.KindPrincipalState},
				ObjectMeta: metav1.ObjectMeta{Name: internalv1.PrincipalStateName(owner), UID: types.UID(uuid.NewString()), Generation: 1, CreationTimestamp: sa.CreationTimestamp},
				Spec:       internalv1.PrincipalStateSpec{Owner: owner, BuiltinAdmin: true},
			}
			store.SetManagedBy(state, store.ManagerSystem)
			if state.Validate() != nil {
				return errors.New("invalid migrated private authority")
			}
			var rvText string
			if err := tx.QueryRowContext(ctx, sqlNextRV, metaResourceVersion).Scan(&rvText); err != nil {
				return err
			}
			rv, err := strconv.ParseUint(rvText, 10, 64)
			if err != nil {
				return err
			}
			state.ResourceVersion = rvText
			data, err := json.Marshal(state)
			if err != nil {
				return errors.New("encode migrated private authority")
			}
			// A conflicting private row is corruption, not an instruction to
			// overwrite trust. The entire migration transaction must roll back.
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM objects WHERE kind=? AND namespace='' AND name=?`, internalv1.KindPrincipalState, state.Name).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return errors.New("private authority migration conflicts with an existing identity")
			}
			if _, err := tx.ExecContext(ctx, sqlUpsertObject, internalv1.KindPrincipalState, "", state.Name, string(state.UID), 1, rv, store.ManagerSystem, state.CreationTimestamp.UTC().Format(time.RFC3339), 0, specHash(state), "{}", 0, string(data)); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO changes(seq,kind,namespace,name,type,resource_version) VALUES (?,?,'',?,'ADDED',?)`, rv, internalv1.KindPrincipalState, state.Name, rvText); err != nil {
				return err
			}
		}
		data, err := json.Marshal(&sa)
		if err != nil {
			return errors.New("encode migrated service account")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE objects SET data=?, spec_hash=? WHERE kind='ServiceAccount' AND namespace=? AND name=?`, string(data), specHash(&sa), sa.Namespace, sa.Name); err != nil {
			return err
		}
	}
	return nil
}
