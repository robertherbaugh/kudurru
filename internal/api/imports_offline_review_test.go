package api

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/importers"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestOfflineUpstreamsReplaceRecursiveGroups(t *testing.T) {
	for _, source := range []string{"adguard", "blocky"} {
		t.Run(source, func(t *testing.T) {
			s, st, host := migrationSettingsFixture(t)
			input := `dns: {upstream_dns: [192.0.2.53]}
clients:
  persistent:
    - name: printer
      ids: [192.0.2.20]
      upstreams: [192.0.2.54]
`
			preview := importers.PreviewAdGuard
			if source == "blocky" {
				input = `upstreams:
  groups:
    default: [192.0.2.53]
    192.0.2.20: [192.0.2.54]
conditional:
  mapping: {corp.example: 192.0.2.55}
`
				preview = importers.PreviewBlocky
			}
			plan, err := preview(t.Context(), importers.OfflineRequest{}, strings.NewReader(input))
			if err != nil {
				t.Fatal(err)
			}
			groups := plan.Settings["upstreams"].(map[string]any)["groups"].(map[string]any)
			wantGroups := 2
			if source == "blocky" {
				wantGroups = 3
			}
			if len(groups) != wantGroups {
				t.Fatalf("generated %d upstream groups, want %d", len(groups), wantGroups)
			}
			// Seed a recursive destination for every generated identity, plus a
			// forwarding group outside the import. This includes default, client
			// and conditional groups, not only a fresh empty destination.
			seed := map[string]any{"keep": map[string]any{"mode": "forward", "servers": []string{"198.51.100.53"}, "strategy": "strict", "timeout": "7s"}}
			for name := range groups {
				seed[name] = map[string]any{"mode": "recursive", "servers": nil, "strategy": "strict"}
			}
			if err := st.Command(t.Context(), st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				prepared, err := PrepareCanonicalSettingsPatch(ctx, tx, host, map[string]any{"upstreams": map[string]any{"groups": seed}})
				if err != nil {
					return err
				}
				return prepared.Apply(ctx, tx)
			}); err != nil {
				t.Fatal(err)
			}
			_, before := migrationSettingsRead(t, st, host)
			for name := range groups {
				if before.Upstreams.Groups[name].Mode != config.UpstreamModeRecursive {
					t.Fatal("destination was not recursive before import")
				}
			}
			plan.Selection = &importers.Selection{Objects: []string{}, Settings: []string{"upstreams"}}
			program, run, result, err := s.currentImportProgram(t.Context(), plan)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < program.Len(); i++ {
				if err := migrationSettingsStep(t.Context(), st, program, i, nil); err != nil {
					t.Fatal(err)
				}
			}
			if (*run).Spec.Status != "completed" || result.SettingsError != "" || result.Settings == nil || result.Settings.Persisted == nil || !*result.Settings.Persisted {
				t.Fatalf("recursive destination import failed: status %s, settings error %s", (*run).Spec.Status, result.SettingsError)
			}
			_, after := migrationSettingsRead(t, st, host)
			for name, expected := range groups {
				got := after.Upstreams.Groups[name]
				want := expected.(map[string]any)
				if got.Mode != config.UpstreamModeForward || !slices.Equal(got.Servers, want["servers"].([]string)) || got.Strategy != want["strategy"] {
					t.Errorf("upstream group %s did not become the imported forwarding group: %+v", name, got)
				}
			}
			if len(after.Upstreams.Groups) != len(groups)+1 || !reflect.DeepEqual(after.Upstreams.Groups["keep"], before.Upstreams.Groups["keep"]) {
				t.Fatal("import changed unrelated upstream groups")
			}
		})
	}
}
