package api

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/importers"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
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

func TestOfflineBlockyRegexDestinationBounds(t *testing.T) {
	for _, alphabet := range []struct{ name, char string }{{"ascii", "a"}, {"unicode", "é"}} {
		for _, length := range []int{1024, 1025} {
			t.Run(fmt.Sprintf("%s/%d", alphabet.name, length), func(t *testing.T) {
				pattern := strings.Repeat(alphabet.char, length)
				allowed := length == 1024
				wantRefs := 0
				if allowed {
					wantRefs = 1
				}
				// Check the actual destination schema, including its Unicode length
				// semantics, independently of the converter before applying its plan.
				candidate := &public.BlockList{ObjectMeta: objectMeta("default", "regex-bound"), Spec: public.BlockListSpec{Sources: []public.ListSource{{Name: "inline", Entries: []public.ListEntry{{Type: public.EntryRegex, Value: pattern}}}}}}
				err := testValidator(t).Validate(t.Context(), candidate, nil)
				if allowed {
					if err != nil {
						t.Fatalf("schema rejected boundary regex: %v", err)
					}
				} else {
					var validationErr *validator.Error
					if !errors.As(err, &validationErr) || !slices.ContainsFunc(validationErr.Causes, func(c validator.Cause) bool {
						return c.Field == "spec.sources[0].entries[0].value" && c.Type == "FieldValueTooLong"
					}) {
						t.Fatalf("expected destination value-length rejection, got %v", err)
					}
				}
				input := "upstreams: {groups: {default: [192.0.2.53]}}\nblocking:\n  denylists:\n    bounded:\n      - |\n        /" + pattern + "/\n        retained.example\n  clientGroupsBlock: {default: [bounded]}\n"
				plan, err := importers.PreviewBlocky(t.Context(), importers.OfflineRequest{}, strings.NewReader(input))
				if err != nil {
					t.Fatal(err)
				}
				s, st, host := migrationSettingsFixture(t)
				program, run, result, err := s.currentImportProgram(t.Context(), plan)
				if err != nil {
					t.Fatal(err)
				}
				for i := 0; i < program.Len(); i++ {
					if err := migrationSettingsStep(t.Context(), st, program, i, nil); err != nil {
						t.Fatal(err)
					}
				}
				for _, item := range result.Items {
					if item.Result == ResultError {
						t.Fatalf("converted %s rejected at apply: %s", item.Kind, item.ErrorKey)
					}
				}
				if (*run).Spec.Status != "completed" || result.JournalError != "" || result.SettingsError != "" || result.Settings == nil || result.Settings.Persisted == nil || !*result.Settings.Persisted {
					t.Fatalf("boundary import did not complete: status %s, settings error %s", (*run).Spec.Status, result.SettingsError)
				}
				_, cfg := migrationSettingsRead(t, st, host)
				if !slices.Equal(cfg.Upstreams.Groups["default"].Servers, []string{"192.0.2.53"}) {
					t.Fatal("converted settings not persisted after object admission")
				}
				entries, lists := 0, 0
				foundRegex, foundRetained := false, false
				for _, item := range result.Items {
					if item.Kind != "BlockList" {
						continue
					}
					lists++
					var list public.BlockList
					if err := st.Get(t.Context(), store.Key{Kind: "BlockList", Namespace: item.Namespace, Name: item.Name}, &list); err != nil {
						t.Fatal(err)
					}
					for _, source := range list.Spec.Sources {
						for _, entry := range source.Entries {
							entries++
							switch {
							case entry.Type == public.EntryRegex && entry.Value == pattern:
								foundRegex = true
							case entry.Type == public.EntryExact && entry.Value == "retained.example":
								foundRetained = true
							default:
								t.Fatal("unexpected or altered persisted entry")
							}
						}
					}
				}
				wantEntries := 1
				if allowed {
					wantEntries++
				}
				if lists != 1 || entries != wantEntries || !foundRetained || foundRegex != allowed {
					t.Fatalf("persisted boundary entries: lists=%d entries=%d regex=%t retained=%t", lists, entries, foundRegex, foundRetained)
				}
				var policy public.Policy
				if err := st.Get(t.Context(), store.Key{Kind: "Policy", Namespace: "default", Name: "default"}, &policy); err != nil {
					t.Fatal(err)
				}
				if len(policy.Spec.BlockListRefs) != wantRefs || len(policy.Spec.AllowListRefs) != 0 {
					t.Fatal("persisted filtering attachment does not reflect complete conversion")
				}
			})
		}
	}
}
