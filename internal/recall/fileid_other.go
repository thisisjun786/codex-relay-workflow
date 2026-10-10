//go:build !unix

package recall

import "os"

// fileIdentity is unavailable here; an empty identity is never compared.
func fileIdentity(os.FileInfo) string { return "" }
