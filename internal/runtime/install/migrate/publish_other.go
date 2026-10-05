//go:build !linux && !darwin

package migrate

import "errors"

// noReplaceSupported says this platform has no rename that refuses to replace, so nothing is published here.
const noReplaceSupported = false

// noReplaceRename has no platform call: the run refuses before writing, and there is no fallback to a replacing rename.
func noReplaceRename(dirfd int, oldName, newName string) error { return errors.ErrUnsupported }
