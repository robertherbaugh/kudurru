// Command rbacgen writes the chart's aggregated ClusterRoles from
// internal/rbac/permissions.yaml (D31): `make rbac-manifests`.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/robertherbaugh/kudurru/internal/rbac"
)

func main() {
	out := flag.String("out", rbac.AggregatedRolesPath, "output file")
	flag.Parse()
	p, err := rbac.LoadPermissions()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, p.RenderAggregatedRoles(), 0o644); err != nil { //nolint:gosec // a chart template, world-readable like the rest of the tree
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
