package main

import (
	"fmt"
	"strings"

	"github.com/robertherbaugh/kudurru/internal/config"
)

const configIntro = `Every settings key of ` + "`kudurru.yaml`" + `, generated from the metadata table in
` + "`internal/config/schema.go`" + `, the same table ` + "`kudurru config schema`" + ` prints and
` + "`GET /api/v1/settings/_schema`" + ` serves.

**Where a value comes from.** Each layer overrides the one before it:

1. the built-in default (the Default column);
2. the config file (` + "`--config`" + `, ` + "`KUDURRU_CONFIG`" + `, default ` + "`/etc/kudurru/kudurru.yaml`" + `);
3. the settings file (` + "`settingsFile`" + ` or ` + "`KUDURRU_SETTINGS_FILE`" + `) for cluster-scope keys,
   which is where changes made in the UI, with ` + "`kudurru settings set`" + ` or with
   ` + "`PATCH /api/v1/settings`" + ` are written; it is watched and hot keys apply at once;
4. the environment: ` + "`KUDURRU_<SECTION>__<KEY>`" + `, with ` + "`__`" + ` for every dot of the path
   (` + "`KUDURRU_LOG__LEVEL=debug`" + `), or ` + "`KUDURRU_<SECTION>__<KEY>_FILE`" + ` naming a file
   that holds the value. ` + "`string[]`" + ` values are comma-separated; ` + "`list`" + ` and ` + "`map`" + ` keys
   can only be set in a file. A key set from the environment is read-only in
   the UI, and a variable that names no key stops the server at start.

**Scope.** ` + "`node`" + ` keys belong to one host. ` + "`cluster`" + ` keys are shared by every
node of a deployment.

**Flags.** *host-only* keys can only be set in the file or the environment
on the host, never through the API or the UI (they name listeners, files and
privileged behaviour). *restart* keys take effect after a restart (in
Kubernetes the chart rolls the pods); every other key is applied while the
server runs. *write-only* keys are never returned by the API.
`

// configReference renders the settings reference.
func configReference() ([]byte, error) {
	var b strings.Builder
	b.WriteString(frontMatter("Settings reference", "Every key of kudurru.yaml with its type, default, scope and environment variable.", 10))
	b.WriteString(generatedNote("internal/config/schema.go"))
	b.WriteString(configIntro)

	type section struct {
		name string
		keys []config.Key
	}
	var sections []*section
	index := map[string]*section{}
	for _, k := range config.Schema {
		name, _, nested := strings.Cut(k.Path, ".")
		if !nested {
			name = "Top-level keys"
		}
		s, ok := index[name]
		if !ok {
			s = &section{name: name}
			index[name] = s
			sections = append(sections, s)
		}
		s.keys = append(s.keys, k)
	}
	if len(sections) == 0 {
		return nil, fmt.Errorf("config.Schema is empty")
	}

	for _, s := range sections {
		fmt.Fprintf(&b, "\n## %s\n\n", s.name)
		b.WriteString("| Key | Type | Default | Scope | Description |\n|---|---|---|---|---|\n")
		for _, k := range s.keys {
			fmt.Fprintf(&b, "| <a id=\"%s\"></a>%s | %s | %s | %s | %s |\n",
				k.Path, code(k.Path), code(string(k.Type)), defaultCell(k), k.Scope, describeKey(k))
		}
	}
	return []byte(b.String()), nil
}

func defaultCell(k config.Key) string {
	if k.WriteOnly {
		return ""
	}
	if s, ok := k.Default.(string); ok {
		if s == "" {
			return "(empty)"
		}
		return code(s) // scalars unquoted; flow quotes inside collections only
	}
	return code(flow(k.Default))
}

// describeKey is the help text, the flags and the environment variable.
func describeKey(k config.Key) string {
	parts := []string{text(k.Help)}
	var flags []string
	if k.HostOnly {
		flags = append(flags, "host-only")
	}
	if k.RestartRequired {
		flags = append(flags, "restart")
	}
	if k.WriteOnly {
		flags = append(flags, "write-only")
	}
	if len(flags) > 0 {
		parts = append(parts, "*"+strings.Join(flags, ", ")+"*")
	}
	if k.Type == config.TypeList || k.Type == config.TypeMap {
		parts = append(parts, "Environment: file only.")
	} else {
		parts = append(parts, "Environment: "+code(envName(k.Path)))
	}
	return strings.Join(parts, "<br>")
}

// envName is the KUDURRU_ variable of a key (internal/config envOverrides).
func envName(path string) string {
	return config.EnvPrefix + strings.ToUpper(strings.ReplaceAll(path, ".", "__"))
}
