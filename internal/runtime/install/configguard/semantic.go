package configguard

import (
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/tomledit"
)

// The writers of this package (the activation, config set and unset, the deactivation and the multi-agent repair) read and
// edit config.toml through internal/tomledit, the semantic lookup and minimal editor every CRW reader of the file agrees with
// (CRW-1141). The line grammar in toml.go is the oracle's, kept as the recorded reference its replay checks; no writer uses it,
// because it misses a quoted or spaced header, a quoted or dotted key and a multi-line array, and appended a second table or
// key that made the whole file unreadable.

// managedKeyPath is the key path of a managed key: its table's dotted path, then the key.
func managedKeyPath(table, key string) []string {
	return append(strings.Split(table, "."), key)
}

// semanticRead is what config.toml holds for table.key.
func semanticRead(content, table, key string) tomledit.Lookup {
	return tomledit.Get(content, managedKeyPath(table, key))
}

// semanticRaw answers the raw value of a key the editor can rewrite, nil when the key is absent, and false when the key is
// there in a form the editor will not touch or the document does not decode.
func semanticRaw(content, table, key string) (*string, bool) {
	look := semanticRead(content, table, key)
	switch look.State {
	case tomledit.Found:
		raw := look.Raw
		return &raw, true
	case tomledit.Absent:
		return nil, true
	}
	return nil, false
}

// errInvalidConfig is the refusal of a config.toml that does not decode: nothing is written to it, and nothing is asked of
// the Codex CLI that would rewrite it.
func errInvalidConfig(path, reason string) error {
	return fmt.Errorf("%s: %s; nothing was changed. Fix the file (codex reads it too) and run the command again", path, reason)
}

// validateConfig refuses a config.toml that does not decode.
func validateConfig(path, content string) error {
	if err := tomledit.Validate(content); err != nil {
		return errInvalidConfig(path, "config.toml is not valid TOML: "+err.Error())
	}
	return nil
}

// semanticEdit maps an edit of internal/tomledit to the result shape of this package. A key the editor will not touch
// answers TomlUnsupportedValue with the reason, and a document that does not decode answers an error.
func semanticEdit(content string, edit tomledit.Edit, err error) (TomlEditResult, string, error) {
	if err != nil {
		if r, ok := tomledit.IsRefusal(err); ok && r.State != tomledit.Invalid {
			return TomlEditResult{Content: content, Action: TomlUnsupportedValue}, r.Reason, nil
		}
		return TomlEditResult{}, "", err
	}
	action := TomlNoop
	if edit.Changed {
		action = TomlUpdated
	}
	return TomlEditResult{Content: edit.Content, PriorValue: edit.Prior, Changed: edit.Changed, Action: action}, "", nil
}

// semanticSet sets table.key to value.
func semanticSet(content, table, key string, value bool) (TomlEditResult, string, error) {
	edit, err := tomledit.Set(content, managedKeyPath(table, key), strconvBool(value))
	return semanticEdit(content, edit, err)
}

// semanticRestore puts table.key back to prior, or removes it when prior is nil.
func semanticRestore(content, table, key string, prior *string) (TomlEditResult, string, error) {
	edit, err := tomledit.Restore(content, managedKeyPath(table, key), prior)
	return semanticEdit(content, edit, err)
}
