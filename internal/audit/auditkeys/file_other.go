//go:build !unix

package auditkeys

import "os"

// Refuse platforms without the protected open and interprocess lock guarantees.
func openProtected(string) (*os.File, error) { return nil, ErrFile }
func lockFile(string) (func(), error)        { return nil, ErrFile }
