package registry

import "database/sql"

func nullString() sql.NullString { return sql.NullString{} }
