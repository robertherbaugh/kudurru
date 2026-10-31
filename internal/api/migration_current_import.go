package api

import (
	"context"
	"encoding/json"
	"errors"

	internal "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/importers"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func saveCurrentMigration(ctx context.Context, tx *sqlite.CommandTx, r *internal.MigrationRun) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(raw) > migrationJournalMaxBytes {
		return store.ErrTooLarge
	}
	obj, err := tx.Apply(ctx, r, store.ApplyOptions{Manager: store.ManagerSystem, CreateOnly: r.ResourceVersion == "", ExpectedResourceVersion: r.ResourceVersion})
	if err == nil {
		*r = *obj.(*internal.MigrationRun)
	}
	return err
}
func currentMigration(ctx context.Context, tx *sqlite.CommandTx, expected *internal.MigrationRun) (*internal.MigrationRun, error) {
	r := &internal.MigrationRun{}
	if err := tx.Get(ctx, store.Key{Kind: internal.KindMigrationRun, Name: expected.Name}, r); err != nil {
		return nil, err
	}
	if r.ResourceVersion != expected.ResourceVersion || r.UID != expected.UID {
		return nil, errPrecondition(expected.ResourceVersion, r.ResourceVersion)
	}
	return r, nil
}
func migrationFailure(item ApplyResult, err error) ApplyResult {
	item.Result = ResultError
	item.Error = truncateMigrationError(err.Error())
	var e *Error
	if errors.As(storeError(err), &e) {
		item.Error = truncateMigrationError(e.Body.Message)
		item.ErrorKey = e.Body.Key
		if h, ok := e.Body.Hint.(string); ok {
			item.Hint = truncateMigrationError(h)
		}
	}
	return item
}

// prepareMigrationDocument uses only the concrete transaction. A validation or
// ownership failure is reportable BEFORE any mutation method can poison tx.
func (s *Server) prepareMigrationDocument(ctx context.Context, tx *sqlite.CommandTx, doc map[string]any, ifMatch string, createOnly bool) (preparedCommandWrite, []rbac.Attributes, error) {
	kind, _ := doc["kind"].(string)
	k, ok := kindByKind(store.Kind(kind))
	if !ok || (kind != "DNSZone" && kind != "DNSRecordSet" && kind != "BlockList" && kind != "ClientGroup" && kind != "Policy") {
		return preparedCommandWrite{}, nil, errUnavailable("current migration kind %s unavailable", kind)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return preparedCommandWrite{}, nil, err
	}
	if len(raw) > MaxObjectBytes {
		return preparedCommandWrite{}, nil, store.ErrTooLarge
	}
	u := &unstructured.Unstructured{}
	if err := json.Unmarshal(raw, &u.Object); err != nil {
		return preparedCommandWrite{}, nil, err
	}
	if u.GetAPIVersion() == "" {
		u.SetAPIVersion(public.GroupVersion.String())
	}
	if u.GetAPIVersion() != public.GroupVersion.String() {
		return preparedCommandWrite{}, nil, errInvalid("unsupported apiVersion")
	}
	if u.GetName() == "" {
		return preparedCommandWrite{}, nil, errInvalid("metadata.name: required")
	}
	if k.namespaced && u.GetNamespace() == "" {
		u.SetNamespace(s.opts.DefaultNamespace)
	}
	if !k.namespaced && u.GetNamespace() != "" {
		return preparedCommandWrite{}, nil, errInvalid("metadata.namespace: %s is cluster-scoped", kind)
	}
	sanitizeDocument(u)
	key := keyFor(k, u.GetNamespace(), u.GetName())
	old := k.newObject()
	if err := tx.Get(ctx, key, old); errors.Is(err, store.ErrNotFound) {
		old = nil
	} else if err != nil {
		return preparedCommandWrite{}, nil, err
	}
	attrs := commandWriteAttributes(k, key, u.Object, old)
	prepared, err := s.prepareCommandWrite(ctx, writeRequest{kind: k, ifMatch: ifMatch, createOnly: createOnly}, store.ManagerAPI, u, key, old)
	if err == nil && old == nil && k.kind == "Policy" {
		err = s.commandPolicyLimit(ctx, tx, key)
	}
	return prepared, attrs, err
}

func (s *Server) currentImportProgram(ctx context.Context, input *importers.Plan) (*MigrationProgram, **internal.MigrationRun, *MigrationResult, error) {
	// Detach the entire reviewed payload before a background worker can observe it.
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(raw) > 64<<20 || len(input.Objects) > 1000 {
		return nil, nil, nil, errInvalid("migration exceeds bounded plan capacity")
	}
	var plan importers.Plan
	if err := json.Unmarshal(raw, &plan); err != nil {
		return nil, nil, nil, err
	}
	run, err := s.buildMigrationRun(ctx, &plan, "")
	if err != nil {
		return nil, nil, nil, err
	}
	_, settings, _ := plan.Selected()
	result := &MigrationResult{RunID: run.Name, Items: []ApplyResult{}}
	program := &MigrationProgram{}
	program.steps = append(program.steps, func(_ context.Context, tx *sqlite.CommandTx) (*MigrationCommandPlan, error) {
		candidate := run.DeepCopy()
		return migrationPlanFor(tx, []rbac.Attributes{migrationSettingsRequirement()}, func(ctx context.Context, tx *sqlite.CommandTx) error { return saveCurrentMigration(ctx, tx, candidate) }, func() { run = candidate }), nil
	})
	for i, doc := range plan.Objects {
		if run.Spec.Items[i].Result == "skipped" {
			continue
		}
		program.steps = append(program.steps, func(ctx context.Context, tx *sqlite.CommandTx) (*MigrationCommandPlan, error) {
			current, err := currentMigration(ctx, tx, run)
			if err != nil {
				return nil, err
			}
			item := ApplyResult{Kind: current.Spec.Items[i].Kind, Namespace: current.Spec.Items[i].Namespace, Name: current.Spec.Items[i].Name}
			if item.Kind == "DNSZone" || item.Kind == "DNSRecordSet" {
				meta, ok := doc["metadata"].(map[string]any)
				if ok {
					ann, ok := meta["annotations"].(map[string]any)
					if !ok {
						ann = map[string]any{}
						meta["annotations"] = ann
					}
					ann[migrationRunAnnotation] = run.Name
				}
			}
			prepared, requirements, prepareErr := s.prepareMigrationDocument(ctx, tx, doc, "", false)
			attrs := append([]rbac.Attributes{migrationSettingsRequirement()}, requirements...)
			return migrationPlanFor(tx, attrs, func(ctx context.Context, tx *sqlite.CommandTx) error {
				if prepareErr != nil {
					item = migrationFailure(item, prepareErr)
				} else {
					wr, err := prepared.apply(ctx, tx)
					if err != nil {
						return err
					}
					item.Result = wr.result
					item.ResourceVersion = wr.obj.GetResourceVersion()
					for j := range current.Spec.Zones {
						z := &current.Spec.Zones[j]
						if item.Kind == "DNSZone" && z.Name == item.Name && z.Namespace == item.Namespace {
							z.UID = string(wr.obj.GetUID())
						}
					}
				}
				current.Spec.Items[i] = migrationItem(item)
				return saveCurrentMigration(ctx, tx, current)
			}, func() { run = current; result.Items = append(result.Items, item) }), nil
		})
	}
	program.steps = append(program.steps, func(ctx context.Context, tx *sqlite.CommandTx) (*MigrationCommandPlan, error) {
		current, err := currentMigration(ctx, tx, run)
		if err != nil {
			return nil, err
		}
		failed := false
		for _, item := range current.Spec.Items[:len(plan.Objects)] {
			if item.Result == ResultError {
				failed = true
			}
		}
		// Settings need a transaction-local canonical planner; never nest the live
		// SettingsWriter inside this command. Preserve explicit failure semantics.
		settingsError := ""
		if !failed && len(settings) > 0 {
			settingsError = "Current migration settings command unavailable; apply the reviewed settings separately."
		}
		for i := len(plan.Objects); i < len(current.Spec.Items); i++ {
			item := &current.Spec.Items[i]
			if item.Result == "skipped" {
				continue
			}
			item.Result = "not_applied"
			item.Hint = "Settings were not changed because a selected object failed or the import stopped."
			if settingsError != "" {
				item.Result = ResultError
				item.Error = settingsError
				item.ErrorKey = KeyUnavailable
			}
		}
		current.Spec.Status = "completed"
		if failed || settingsError != "" {
			current.Spec.Status = "partial"
		}
		now := metav1.NewTime(s.opts.Now())
		current.Spec.FinishedAt = &now
		current.Spec.SettingsError = settingsError
		return migrationPlanFor(tx, []rbac.Attributes{migrationSettingsRequirement()}, func(ctx context.Context, tx *sqlite.CommandTx) error { return saveCurrentMigration(ctx, tx, current) }, func() { run = current; result.SettingsError = settingsError }), nil
	})
	return program, &run, result, nil
}

func (s *Server) applyCurrentMigration(ctx context.Context, input *importers.Plan) (*MigrationResult, error) {
	ctx, cancel := context.WithTimeout(ctx, migrationTimeout)
	defer cancel()
	program, _, result, err := s.currentImportProgram(ctx, input)
	if err != nil {
		return nil, err
	}
	next, err := s.admitMigration(ctx, program)
	if err != nil {
		return nil, err
	}
	for i := 0; i < program.Len(); i++ {
		if err := next(ctx); err != nil {
			if i == 0 {
				return nil, storeError(err)
			}
			result.JournalError = "Import stopped before further writes: " + truncateMigrationError(err.Error())
			break
		}
	}
	return result, nil
}

// All destination content is fixed before admission. No credentials, mutable
// request state or callbacks from a client are placed in the durable journal.
func (s *Server) startCurrentMigration(ctx context.Context, input *importers.Plan) (*MigrationRun, error) {
	program, run, _, err := s.currentImportProgram(ctx, input)
	if err != nil {
		return nil, err
	}
	if program.Len() == 2 && len(input.Settings) == 0 {
		return nil, errInvalid("Select at least one object or settings section to import")
	}
	s.imports.mu.Lock()
	defer s.imports.mu.Unlock()
	if s.imports.closed || len(s.imports.cancels) >= maxMigrationWorkers {
		return nil, errUnavailable("Import workers are busy or shutting down; review migration history before starting another import")
	}
	next, err := s.admitMigration(ctx, program)
	if err != nil {
		return nil, err
	}
	if err := next(ctx); err != nil {
		return nil, storeError(err)
	}
	view := migrationView(*run)
	state := *stateFrom(ctx)
	workCtx, cancel := context.WithTimeout(withState(context.WithoutCancel(ctx), &state), migrationTimeout)
	principal, ok := auth.PrincipalFrom(workCtx)
	workCtx = audit.WithRequest(workCtx, actorOf(principal, ok), audit.Request{ID: state.id, IP: state.clientIP.String(), Method: state.method, Path: state.path, UA: state.userAgent}, func() string { return state.authz })
	if s.imports.cancels == nil {
		s.imports.cancels = map[string]context.CancelFunc{}
	}
	id := (*run).Name
	s.imports.cancels[id] = cancel
	s.imports.wg.Add(1)
	go func() {
		defer s.imports.wg.Done()
		defer func() { cancel(); s.imports.mu.Lock(); delete(s.imports.cancels, id); s.imports.mu.Unlock() }()
		defer func() {
			if recover() != nil {
				s.log.Error("current migration stopped unexpectedly", "run", id)
			}
		}()
		for i := 1; i < program.Len(); i++ {
			if err := next(workCtx); err != nil {
				s.log.Warn("current migration stopped; prior journaled writes retained", "run", id, "error", err)
				return
			}
		}
	}()
	return &view, nil
}
