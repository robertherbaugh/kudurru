//go:build !unix

package main

import "os"

// Existing path aliases are still checked by os.SameFile. Platforms without
// Unix access/link metadata retain the database opener's ACL enforcement.
func storageAccess(_ string, _ os.FileInfo) error { return nil }
