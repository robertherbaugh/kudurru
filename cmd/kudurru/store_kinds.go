package main

import (
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func privateStoreKinds() []store.Kind {
	kinds := runtimekinds.Private()
	out := make([]store.Kind, len(kinds))
	for i, k := range kinds {
		out[i] = store.Kind(k)
	}
	return out
}
