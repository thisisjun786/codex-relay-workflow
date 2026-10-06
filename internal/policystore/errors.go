package policystore

import (
	"errors"
)

// errNoCodexHome is the reason a reading gives when neither CODEX_HOME nor HOME names a home.
var errNoCodexHome = errors.New("neither CODEX_HOME nor HOME names a Codex home")
