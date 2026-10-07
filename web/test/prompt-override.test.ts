// New in CRW (no CXC counterpart): the prompt-override editor's two-state transitions, tested
// without a DOM. The ported editor collapsed the store's null (inherit) and "" (an override that
// is empty) into one blank textarea, so clearing an override silently inherited and an empty
// override could not be stored. These tests build the save body through the editor state
// function, which is what the issue's acceptance criteria require (a test that passes a value
// straight to setHelperRole does not exercise the editor).
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  editText,
  inheritState,
  isDirty,
  saveBody,
  storedToState,
} from "../src/prompt-override.ts";

test("clearing a stored override saves the empty string, not inherit", () => {
  // reviewer held "text"; the user selects the override state, deletes every character and saves.
  const draft = editText("");
  assert.deepEqual(saveBody(draft), { promptOverride: "" });
  assert.notDeepEqual(saveBody(draft), { promptOverride: null });
});

test("a role stored as inherit can be given an empty-string override", () => {
  // storedToState(null) is the inherit state; entering override and saving an empty text stores "".
  assert.deepEqual(storedToState(null), { kind: "inherit" });
  assert.deepEqual(saveBody(editText("")), { promptOverride: "" });
});

test("only the explicit switch to inherit sends null", () => {
  assert.deepEqual(saveBody(inheritState()), { promptOverride: null });
  // A draft that had text still sends null once the user switches to inherit explicitly.
  assert.deepEqual(saveBody(inheritState()), { promptOverride: null });
  assert.notDeepEqual(saveBody(editText("text")), { promptOverride: null });
});

test("whitespace-only text is sent verbatim, with no trim", () => {
  assert.deepEqual(saveBody(editText("   ")), { promptOverride: "   " });
  assert.deepEqual(saveBody(editText("  keep  ")), { promptOverride: "  keep  " });
});

test("dirty compares the draft against the stored value", () => {
  // inherit against a stored null is the stored state; inherit against a stored string is a change.
  assert.equal(isDirty(storedToState(null), null), false);
  assert.equal(isDirty(storedToState(""), ""), false);
  assert.equal(isDirty(inheritState(), ""), true);
  // an override whose text equals the stored string is not a change; a different text is.
  assert.equal(isDirty(editText("same"), "same"), false);
  assert.equal(isDirty(editText("other"), "same"), true);
  // an override that becomes empty against a stored non-empty string is a change (it stores "").
  assert.equal(isDirty(editText(""), "text"), true);
});

test("the stored value round-trips through the editor state", () => {
  assert.deepEqual(storedToState(""), { kind: "override", text: "" });
  assert.deepEqual(saveBody(storedToState("")), { promptOverride: "" });
  assert.deepEqual(saveBody(storedToState("text")), { promptOverride: "text" });
  assert.deepEqual(saveBody(storedToState(null)), { promptOverride: null });
  // Discard is this same function: the stored value restored verbatim, empty string included.
  assert.equal(isDirty(storedToState(""), ""), false);
});
