package license

import "path/filepath"

// The environment variables and file that carry a key. The key is a
// signature, not a secret that grants access, but it names the licensee, so
// prefer the _FILE form with a Secret or a Docker secret.
const (
	// EnvKey holds the key itself.
	EnvKey = "KUDURRU_LICENSE_KEY"
	// EnvKeyFile names a file that holds the key.
	EnvKeyFile = "KUDURRU_LICENSE_KEY_FILE"
	// FileName is the key file inside node.dataDir.
	FileName = "license.key"
)

// DefaultSources returns where a node looks for its key, first match wins:
// the --license-key flag, KUDURRU_LICENSE_KEY, the file KUDURRU_LICENSE_KEY_FILE
// names, then <dataDir>/license.key. dataDir may be empty to skip the last.
func DefaultSources(flagKey, dataDir string, getenv func(string) string) []Source {
	src := []Source{
		Literal("--license-key", flagKey),
		Literal("env "+EnvKey, getenv(EnvKey)),
	}
	if p := getenv(EnvKeyFile); p != "" {
		src = append(src, File("file "+p, p))
	}
	if dataDir != "" {
		p := filepath.Join(dataDir, FileName)
		src = append(src, File("file "+p, p))
	}
	return src
}
