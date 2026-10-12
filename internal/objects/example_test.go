package objects_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/robertherbaugh/kudurru/internal/bootstrap"
	"github.com/robertherbaugh/kudurru/internal/objects"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
)

// Example shows the serve start-up order: open the store, seed the first
// boot defaults, load the file-managed objects, then start zonecache and
// the watch loop.
func Example() {
	ctx := context.Background()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	etc, _ := os.MkdirTemp("", "kudurru")
	defer func() { _ = os.RemoveAll(etc) }()
	dir := filepath.Join(etc, "objects.d") // cfg.ObjectsDir
	_ = os.Mkdir(dir, 0o750)
	_ = os.WriteFile(filepath.Join(dir, "policy.yaml"), []byte(`
apiVersion: kudurru.io/v1alpha1
kind: Policy
metadata: { name: default }
spec: { clientGroupRefs: [default], blockListRefs: [ads] }
`), 0o600)

	st := memory.New() // 1. open the store (sqlite or kube in serve)
	defer func() { _ = st.Close() }()

	seeded, _ := bootstrap.Seed(ctx, st, bootstrap.Options{Logger: quiet}) // 2. first boot
	fmt.Println("first boot:", seeded.FirstBoot, "created:", len(seeded.Created))

	loader, _ := objects.New(objects.Options{Store: st, Dir: dir, Logger: quiet}) // 3. files
	rep := loader.Load(ctx)
	for _, d := range rep.Diagnoses {
		fmt.Println(d.Severity, d.Reason, d.Object, "was", d.Owner)
	}

	pol, _ := st.List(ctx, "Policy", store.ListOptions{})
	fmt.Println("Policy/default managed by", store.ManagedBy(pol.Items[0]))
	// 4. zonecache.Start(...); go loader.Watch(ctx)

	// Output:
	// first boot: true created: 3
	// info TakenOver Policy/default/default was bootstrap
	// Policy/default managed by file:objects.d/policy.yaml
}
