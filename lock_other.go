//go:build !unix

package reversible

import "os"

// Locking is advisory and only implemented on Unix-like systems for now.
func lockFile(*os.File) error   { return nil }
func unlockFile(*os.File) error { return nil }
