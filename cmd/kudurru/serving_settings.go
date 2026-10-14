package main

import (
	"context"
	"encoding/json"
	"errors"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// initializeServingSettings migrates the primary's existing effective public
// settings exactly once. Startup already owns the actual writable command;
// followers receive this object from replication. Later host restarts must not
// overwrite canonical cluster configuration or export local environment locks.
func initializeServingSettings(ctx context.Context, tx *sqlite.CommandTx, cfg *config.Config) error {
	if tx == nil || cfg == nil {
		return errors.New("serving settings: missing startup input")
	}
	var current public.Kudurru
	err := tx.Get(ctx, store.Key{Kind: "Kudurru", Name: "default"}, &current)
	if err == nil {
		return nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	next := &public.Kudurru{ObjectMeta: metav1.ObjectMeta{Name: "default"}, Spec: public.KudurruSpec{Settings: map[string]apiextensionsv1.JSON{}}}
	for key, value := range config.ClusterSeed(cfg) {
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		next.Spec.Settings[key] = apiextensionsv1.JSON{Raw: raw}
	}
	_, err = tx.Apply(ctx, next, store.ApplyOptions{Manager: store.ManagerSystem, CreateOnly: true})
	return err
}
