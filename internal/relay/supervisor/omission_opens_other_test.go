//go:build !linux

package supervisor

import "testing"

// watchDatabaseOpens reports that this host cannot watch the database file; the tests that use it then skip
// the check that no second connection is opened.
func watchDatabaseOpens(testing.TB, string) (func() int, bool) { return nil, false }
