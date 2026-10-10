package role

import (
	"encoding/json"
	"testing"
)

// CRW-1120 (known-defects.md, "Found by the helper role spawn resolution and settings API port", item beginning "`updateSettings`
// publishes"): the answer to an update describes the settings that update's protected write produced, not the state another writer
// made once the lock was released. Both writes are kept.
func TestUpdateSettingsAnswersWithItsOwnWrite(t *testing.T) {
	t.Run("set", func(t *testing.T) {
		env, _ := home(t)
		other := func() {
			must(UpdateSettings(env, json.RawMessage(`{"role":"reviewer","mode":"model","model":"other/writer"}`)))
		}
		got, err := updateSettings(env, json.RawMessage(`{"role":"explorer","mode":"model","model":"mine/first"}`), other)
		check(t, err)
		if m := got.Roles[Explorer].Model; m == nil || *m != "mine/first" || !got.Overrides[Explorer] {
			t.Fatalf("explorer in its own answer = %+v", got.Roles[Explorer])
		}
		if got.Overrides[Reviewer] || got.Roles[Reviewer].Mode != ModeDefault {
			t.Fatalf("the answer describes another writer's reviewer: %+v", got.Roles[Reviewer])
		}
		if now := must(ReadSettings(env)); !now.Overrides[Explorer] || !now.Overrides[Reviewer] {
			t.Fatalf("a write was lost: %+v", now.Overrides)
		}
	})
	t.Run("reset", func(t *testing.T) {
		env, _ := home(t)
		must(UpdateSettings(env, json.RawMessage(`{"role":"explorer","mode":"model","model":"mine/first"}`)))
		must(UpdateSettings(env, json.RawMessage(`{"role":"reviewer","mode":"model","model":"mine/second"}`)))
		got, err := updateSettings(env, json.RawMessage(`{"role":"explorer","inherit":true}`), func() {
			must(UpdateSettings(env, json.RawMessage(`{"role":"executor","mode":"model","model":"other/writer"}`)))
		})
		check(t, err)
		if got.Overrides[Explorer] || !got.Overrides[Reviewer] {
			t.Fatalf("reset answer lost its own state: %+v", got.Overrides)
		}
		if got.Overrides[Executor] {
			t.Fatalf("the reset answer describes another writer's executor: %+v", got.Roles[Executor])
		}
		if now := must(ReadSettings(env)); !now.Overrides[Executor] || now.Overrides[Explorer] {
			t.Fatalf("a write was lost: %+v", now.Overrides)
		}
	})
}
