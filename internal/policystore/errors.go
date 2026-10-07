package policystore

import (
	"errors"
)

// errNoCodexHome is the reason a reading gives when neither CODEX_HOME nor HOME names a home.
var errNoCodexHome = errors.New("neither CODEX_HOME nor HOME names a Codex home")

// errUnencodablePath is the reason a reading gives for a policy path this host cannot spell as the
// bytes the OS opens, which is what the launcher does with pyvalue.FSEncode.
var errUnencodablePath = errors.New("the execution policy path cannot be encoded for this system")
