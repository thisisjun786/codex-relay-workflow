package configguard

import "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"

// ManagedKey is a non-feature config key from CXC v0.2.40 config-guard/src/managed-keys.ts.
type ManagedKey struct {
	Table, Key string
	AutoEnable bool
	Caution    string
}

// ConfigManagedKeys returns the closed whitelist. AutoEnable is decided per entry.
func ConfigManagedKeys() []ManagedKey {
	return []ManagedKey{{
		Table:      "memories",
		Key:        "dedicated_tools",
		AutoEnable: true,
		Caution: "memories/{list,read,search,add_ad_hoc_note} 네 도구가 열립니다. " +
			"add_ad_hoc_note는 메모리에 새 노트를 만드는 쓰기 경로이지만, " +
			"crw 의 PreToolUse 메모리 쓰기 게이트가 명시 요청 없는 쓰기를 거부합니다. " +
			"설치가 이 키를 켜고, 'crw disable' 이 설치 이전 값으로 되돌립니다.",
	}}
}

// ManagedKeyID is the dotted identity; the oracle performs no validation here.
func ManagedKeyID(entry ManagedKey) string { return entry.Table + "." + entry.Key }

func AutoEnabledManagedKeys() []ManagedKey {
	result := make([]ManagedKey, 0)
	for _, entry := range ConfigManagedKeys() {
		if entry.AutoEnable {
			result = append(result, entry)
		}
	}
	return result
}

// FindManagedKey returns nil for an id outside the whitelist, after JavaScript trim.
func FindManagedKey(id string) *ManagedKey {
	wanted := text.Trim(id)
	for _, entry := range ConfigManagedKeys() {
		if ManagedKeyID(entry) == wanted {
			return &entry
		}
	}
	return nil
}
