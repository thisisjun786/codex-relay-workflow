package configguard

import (
	"reflect"
	"testing"
	"unicode/utf8"
)

// Pinned managed-keys.ts:70-74 with the authorized codexclaw/cxc -> crw substitution.
const managedCaution = "memories/{list,read,search,add_ad_hoc_note} 네 도구가 열립니다. " +
	"add_ad_hoc_note는 메모리에 새 노트를 만드는 쓰기 경로이지만, " +
	"crw 의 PreToolUse 메모리 쓰기 게이트가 명시 요청 없는 쓰기를 거부합니다. " +
	"설치가 이 키를 켜고, 'crw disable' 이 설치 이전 값으로 되돌립니다."

func TestConfigManagedKeysPolicy(t *testing.T) {
	// Ported from config-guard/test/toml-edit.test.ts:162-171.
	keys := ConfigManagedKeys()
	want := []ManagedKey{{Table: "memories", Key: "dedicated_tools", AutoEnable: true, Caution: managedCaution}}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
	for _, key := range keys {
		if utf8.RuneCountInString(key.Caution) <= 20 {
			t.Fatalf("%s has no substantive caution", ManagedKeyID(key))
		}
	}
	if ManagedKeyID(keys[0]) != "memories.dedicated_tools" || FindManagedKey("memories.dedicated_tools") == nil || FindManagedKey("tools.dangerous") != nil {
		t.Fatal("managed identity or whitelist lookup differs")
	}
}

func TestAutoEnabledManagedKeys(t *testing.T) {
	// Ported from config-guard/test/toml-edit.test.ts:173-179.
	keys := AutoEnabledManagedKeys()
	if len(keys) != 1 || ManagedKeyID(keys[0]) != "memories.dedicated_tools" || !keys[0].AutoEnable {
		t.Fatalf("auto-enabled = %v", keys)
	}
	if key := FindManagedKey("memories.dedicated_tools"); key == nil || !key.AutoEnable {
		t.Fatalf("lookup = %v", key)
	}
}

func TestFindManagedKeyRecordedEdges(t *testing.T) {
	for _, id := range []string{"memories.dedicated_tools", " \ufeffmemories.dedicated_tools\u3000"} {
		got := FindManagedKey(id)
		want := ManagedKey{Table: "memories", Key: "dedicated_tools", AutoEnable: true, Caution: managedCaution}
		if got == nil || *got != want {
			t.Fatalf("find %q = %v", id, got)
		}
	}
	for _, id := range []string{"\u0085memories.dedicated_tools", "Memories.dedicated_tools", "tools.dangerous", "memories. dedicated_tools", ""} {
		if got := FindManagedKey(id); got != nil {
			t.Fatalf("find %q = %v, want nil", id, got)
		}
	}
}

func TestManagedKeyIDRecordedEdges(t *testing.T) {
	if got := ManagedKeyID(ManagedKey{}); got != "." {
		t.Fatalf("empty identity = %q", got)
	}
	if got := ManagedKeyID(ManagedKey{Table: "a.b", Key: "c.d"}); got != "a.b.c.d" {
		t.Fatalf("dotted identity = %q", got)
	}
}

func TestManagedMetadataFresh(t *testing.T) {
	keys, auto, found := ConfigManagedKeys(), AutoEnabledManagedKeys(), FindManagedKey("memories.dedicated_tools")
	keys[0].AutoEnable, auto[0].Key, found.Caution = false, "other", "other"
	if !ConfigManagedKeys()[0].AutoEnable || AutoEnabledManagedKeys()[0].Key != "dedicated_tools" || FindManagedKey("memories.dedicated_tools").Caution != managedCaution {
		t.Fatal("caller mutation changed managed metadata")
	}
}

func TestManagedKeyUsesExistingTomlEditor(t *testing.T) {
	key := FindManagedKey("memories.dedicated_tools")
	input := "[memories]\ngenerate_memories = true\n[other]\nkeep = 7\n"
	set := SetTableKey(input, key.Table, key.Key, key.AutoEnable)
	if value, found := ReadTableKey(set.Content, key.Table, key.Key); !set.Changed || !found || value != "true" {
		t.Fatalf("set = %+v, value = %q (found %v)", set, value, found)
	}
	restored := RestoreTableKey(set.Content, key.Table, key.Key, set.PriorValue)
	if restored.Content != input {
		t.Fatalf("round trip = %q, want %q", restored.Content, input)
	}
}
