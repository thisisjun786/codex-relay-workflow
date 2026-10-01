package managed

import (
	"fmt"
	"sort"
)

// maxProfileKeys bounds the object form of expectedPermissionProfile. Codex 0.154 reports two keys,
// id and extends; the rest of the room is for a host that adds a field, which the relay compares
// whole and untyped, so the profile is carried as the host spelled it and not as a fixed shape.
const maxProfileKeys = 16

// permissionProfile checks the expectedPermissionProfile a request's settings name. The relay compares
// the recorded value with the profile a host reports as one whole value (registry TaskSettings.Mismatches),
// and Codex 0.154 reports an object, so the field is that object or the text it has always been
// (a record that names no profile takes the sandbox's built-in one, and null stays that).
//
// The object carries an id, text; an extends, null or text; and any other key a host adds, as text, a
// boolean or null. Nothing nested, and no number: a request decodes a number to a float64, so a large
// integer would be stored rounded, and a host's report keeps its own spelling of one, which the
// comparison spells differently, so one profile would be two values to it. An absent extends and a null
// one are different objects to that comparison, so neither is added or removed here.
func permissionProfile(value any, at string) error {
	object, ok := value.(map[string]any)
	if !ok {
		if _, isText := value.(string); !isText {
			return fmt.Errorf("%s must be nonblank text or an object", at)
		}
		return text(value, at, 500)
	}
	if len(object) > maxProfileKeys {
		return fmt.Errorf("%s must name at most %d keys", at, maxProfileKeys)
	}
	id, ok := object["id"]
	if !ok {
		return fmt.Errorf("%s.id is required", at)
	}
	if err := text(id, at+".id", 500); err != nil {
		return err
	}
	if extends := object["extends"]; extends != nil {
		if err := text(extends, at+".extends", 500); err != nil {
			return err
		}
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := text(key, at+" key", 500); err != nil {
			return err
		}
		switch member := object[key].(type) {
		case nil, bool:
		case string:
			if err := text(member, at+"."+key, 500); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s.%s must be text, a boolean or null", at, key)
		}
	}
	return nil
}
