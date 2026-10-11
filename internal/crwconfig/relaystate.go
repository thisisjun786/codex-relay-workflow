package crwconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// ConfiguredRelayState is the durable relay state root the configuration file names (paths.relay_state),
// or "" when the file names none. It is the one reading every consumer of the relay's state selection
// shares, so the management tool, a relay started directly, the registered services and the install
// commands all land on the same bundle for one configuration and one socket. The root is the directory
// whose children are the relay's per-scope store directories; the caller joins the scope.
//
// A paths key that points a part of the state apart is refused first, whether or not relay_state is
// named beside it, and a root that fails a structural check is refused with the check's words, so
// nothing opens a store against a split. A host that cannot name its configuration location at all
// has no configured root.
func ConfiguredRelayState(getenv func(string) string) (string, error) {
	file, err := loadOptional(getenv, "")
	if err != nil || file == nil {
		return "", err
	}
	if err := file.RelaySplit(); err != nil {
		return "", err
	}
	root := file.roots[RootRelayState]
	if root.Source != SourceConfig {
		return "", nil
	}
	if err := file.Refusal(getenv, RootRelayState); err != nil {
		return "", err
	}
	return root.Path, nil
}

// RelayStateSplit refuses a configuration whose paths section points a part of the relay's durable
// state apart (state_split), and a configuration that cannot be read; it judges nothing else. Every
// source of the relay's state selection calls it (--state and CODEX_SESSION_RELAY_STATE as well as
// the configured and default bases): a split configuration is wrong whichever store is chosen, while
// a relay_state the selection does not use is not judged.
func RelayStateSplit(getenv func(string) string) error {
	file, err := loadOptional(getenv, "")
	if err != nil || file == nil {
		return err
	}
	return file.RelaySplit()
}

// RelaySplit is the state_split refusal of this file's paths section, nil when it names no split key.
func (f *File) RelaySplit() error {
	report := Report{Findings: []Finding{}}
	checkRelayKeys(f, func(level Level, root, check, format string, args ...any) {
		report.Findings = append(report.Findings, Finding{Level: level, Root: root, Check: check, Message: fmt.Sprintf(format, args...)})
	})
	return report.Refusal()
}

// RelayLogFile is the daemon log of the relay store at state when the configuration file names a
// log_root, and "" when it does not, in which case the log stays in the store directory beside the
// rest of the store as it always has. Each store has a directory of its own below
// <log_root>/relay, named by the first 16 hex digits of the SHA-256 of the store's path, so two
// stores never write one log and the same store always finds its own. A log_root that fails a
// structural check is refused, so a service does not start writing a log it cannot keep.
func RelayLogFile(getenv func(string) string, state string) (string, error) {
	file, err := loadOptional(getenv, "")
	if err != nil || file == nil {
		return "", err
	}
	root := file.roots[RootLog]
	if root.Source != SourceConfig {
		return "", nil
	}
	if err := file.Refusal(getenv, RootLog); err != nil {
		return "", err
	}
	return relayLogFileBelow(root.Path, state), nil
}

// relayLogFileBelow is the daemon log of the store at state below the log root logRoot.
func relayLogFileBelow(logRoot, state string) string {
	sum := sha256.Sum256([]byte(state))
	return rootJoin(logRoot, "relay", hex.EncodeToString(sum[:8]), RelayLogName)
}

// RelayLogName is the relay daemon log's file name, in the store directory or below log_root.
const RelayLogName = "daemon.log"
