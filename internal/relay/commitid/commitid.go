// Package commitid is the one definition of when two stored commit identities name the same commit. Go code
// and SQL both use it: Same is the Go definition, and the SQL function crw_same_commit(a, b) is registered
// with the sqlite driver here, so a query compares a head column through the same rule instead of trimming
// and lower-casing it by hand (the SQL trim only removes ASCII spaces, which is why a head stored with a
// no-break or ideographic space was missed).
package commitid

import (
	"database/sql/driver"
	"strings"

	"modernc.org/sqlite"
)

// SQLFunction is the name of the scalar function a query calls to compare two commit identities.
const SQLFunction = "crw_same_commit"

// Same reports whether a and b name the same commit: each is trimmed of surrounding Unicode whitespace and
// lower-cased, and an empty identity names no commit, so it is equal to nothing.
func Same(a, b string) bool {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	return a != "" && a == b
}

// Register installs the function on one driver instance. The store opens its databases through a driver it
// builds itself, and a function registered on another instance is not visible to those connections, so each
// place that builds a driver calls Register on it.
func Register(d *sqlite.Driver) error {
	return d.RegisterDeterministicScalarFunction(SQLFunction, 2, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		if Same(commitText(args[0]), commitText(args[1])) {
			return int64(1), nil
		}
		return int64(0), nil
	})
}

// commitText is the text of a SQL argument. Only TEXT names a commit: a BLOB, a number or NULL names none.
func commitText(v driver.Value) string {
	if x, ok := v.(string); ok {
		return x
	}
	return ""
}
