package dnsdist

import "os"

// configState owns one configuration directory through the complete child and
// worker lifetime. A retained bypass identity is local policy, not evidence of
// a running child or permission for a backend to activate managed roots.
type configState struct {
	dir      *os.File
	bypassID string
	runID    string
}

func (s *configState) close() error { return s.dir.Close() }

const managedBypassFile = "managed-cache-bypass.state"
const managedBypassHeader = "kudurru/edge-managed-cache-bypass/v1\n" //nolint:gosec // public file-format tag, not a credential
