//go:build !unix

package memberidentity

// Refuse platforms without the protected descriptor-relative file operations.
func createFile(string, []byte) error { return ErrIdentity }
func readFile(string) ([]byte, error) { return nil, ErrIdentity }
