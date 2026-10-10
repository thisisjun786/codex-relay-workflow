package configguard

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/thisisjun786/codex-relay-workflow/internal/tomledit"
)

// The pending entries of an intent and the flag state a done record carries (CRW-1153; see the top of intent.go).

// configFlagState answers the fingerprint of config.toml and the state of flag name in it: on, off (absent included), or nil
// when the file holds it in a form crw does not read.
func configFlagState(path, name string) (string, *bool, error) {
	b, exists, err := activationReadFile(path)
	if err != nil {
		return "", nil, err
	}
	off := false
	if !exists {
		return configAbsent, &off, nil
	}
	return fingerprintBytes(b), flagOnIn(string(b), name), nil
}

func flagOnIn(content, name string) *bool {
	live, editable := semanticRaw(content, "features", name)
	if !editable {
		return nil
	}
	on := live != nil && (*live == "true" || tomledit.SameValue(*live, "true"))
	return &on
}

// readPending answers the pending entries the intent holds (nil without one) and the intent itself.
func readPending(home string) (*installIntent, error) {
	raw, exists, err := activationReadFile(intentPath(home))
	if err != nil || !exists {
		return nil, err
	}
	var in installIntent
	if err := json.Unmarshal(raw, &in); err != nil || in.Version != 1 {
		return nil, fmt.Errorf("an interrupted crw change left %s, which crw cannot read; nothing was changed. Inspect it, then remove it to continue", intentPath(home))
	}
	in.home = home
	return &in, nil
}

// pendingKeyNote is what a command that leaves key id alone says when the key is pending: the uncertainty, and the next step.
func pendingKeyNote(home, id string) string {
	in, err := readPending(home)
	if err != nil || in == nil {
		return ""
	}
	for _, e := range in.Pending {
		if e.Kind == intentKey && e.Name == id {
			return " An interrupted crw change may have written " + id + " = " + e.Applied + ", and whether it did is unknown, so crw keeps that pending in " + intentPath(home) +
				" and does not treat the value as its own. If the value is not wanted, edit config.toml by hand; once the key no longer holds it the pending note clears itself. " +
				"To keep the value as yours, run 'crw install config unset " + id + " --release'."
		}
	}
	return ""
}

// releasePending drops the pending entry of key id, the explicit resolution of 'crw install config unset <key> --release'. It
// answers whether there was one. The caller holds the config lock and has run the recovery.
func releasePending(home, id string, unsynced *error) (bool, error) {
	in, err := readPending(home)
	if err != nil || in == nil {
		return false, err
	}
	kept := slices.DeleteFunc(slices.Clone(in.Pending), func(e intentEffect) bool { return e.Kind == intentKey && e.Name == id })
	if len(kept) == len(in.Pending) {
		return false, nil
	}
	in.Pending = kept
	return true, in.close(unsynced)
}
