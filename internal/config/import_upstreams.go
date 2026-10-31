package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
)

// importUpstreams expands the file-owned layer before UI ownership and the
// environment. It never runs when a stored UI section owns the declarations.
func importUpstreams(k *koanf.Koanf, source, path string, defaultDeclared bool) error {
	if path == "" {
		return nil
	}
	resolved := path
	if !filepath.IsAbs(resolved) && source != "" {
		resolved = filepath.Join(filepath.Dir(source), resolved)
	}
	f, err := os.Open(resolved) //nolint:gosec // operator-selected host-only configuration source
	if err != nil {
		return fmt.Errorf("upstreams.importFile: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > DnsmasqMaxBytes {
		return fmt.Errorf("upstreams.importFile: require a regular file of at most 4 MiB")
	}
	parsed, err := ParseDnsmasq(f)
	if err != nil {
		return fmt.Errorf("upstreams.importFile: %w", err)
	}
	if len(parsed.Issues) > 0 {
		var issues []string
		for _, issue := range parsed.Issues {
			issues = append(issues, fmt.Sprintf("line %d: %s", issue.Line, issue.Reason))
		}
		return fmt.Errorf("upstreams.importFile: no rules applied: %s", strings.Join(issues, "; "))
	}
	if len(parsed.Default) > 0 {
		if defaultDeclared {
			return fmt.Errorf("upstreams.importFile: global servers conflict with explicitly configured default group")
		}
		parsed.Groups["default"] = UpstreamGroup{Servers: parsed.Default, Strategy: "parallel_best"}
	}
	for name := range parsed.Groups {
		if name != "default" && k.Exists("upstreams.groups."+name) {
			return fmt.Errorf("upstreams.importFile: imported group %q already exists", name)
		}
	}
	// Compare expanded suffixes, so alternate bracket/case/trailing-dot forms
	// cannot silently replace a declaration in the node/settings file.
	if conditional, ok := k.Get("upstreams.conditional").(map[string]any); ok {
		for key := range conditional {
			domains := []string{key}
			if strings.HasPrefix(key, "[/") && strings.HasSuffix(key, "/]") {
				domains = strings.Split(key[2:len(key)-2], "/")
			}
			for _, domain := range domains {
				if domain == "" {
					continue
				}
				normalized := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), ".")) + "."
				if _, exists := parsed.Conditional[normalized]; exists {
					return fmt.Errorf("upstreams.importFile: imported suffix conflicts with configured conditional routing")
				}
			}
		}
	}
	// Convert typed values into koanf's wire map; its replacement merge avoids
	// inheriting fields from the compiled default group.
	raw, err := json.Marshal(map[string]any{"upstreams": map[string]any{"groups": parsed.Groups, "conditional": parsed.Conditional}})
	if err != nil {
		return err
	}
	var layer map[string]any
	if err := json.Unmarshal(raw, &layer); err != nil {
		return err
	}
	return k.Load(confmap.Provider(layer, "."), nil, koanf.WithMergeFunc(mergeLayer))
}

func declaresDefaultUpstream(raw map[string]any) bool {
	groups, ok := subMap(raw, "upstreams", "groups")
	if !ok {
		return false
	}
	_, declared := groups["default"]
	return declared
}
