//go:build dev

package stopevents

import (
	"bytes"
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/pyload"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// One test per reader property of the per-event judge (SEV-5..SEV-14). SEV-1..4, the writer's
// own, are the Go hook's tests (internal/relay/hook) and the corpus's.

// SEV-5: TRUE, and the counters beside it.
func TestSEV05_EveryEventAcceptedOnceReadsTrue(t *testing.T) {
	t.Run("a complete event", func(t *testing.T) {
		h, _ := oneEvent(t)
		code, answer := verify(t, roots(h.journal)...)
		expectVerdict(t, code, answer, 0, "TRUE")
	})
	t.Run("a turn of three Stops is three events where the old count saw duplicates", func(t *testing.T) {
		h := newHost(t, hook.Release)
		document := distinctThird(t)
		for _, index := range []int{0, 2, 4} {
			h.twice(h.at(document, index))
		}
		code, answer := verify(t, roots(h.journal)...)
		expectVerdict(t, code, answer, 0, "TRUE")
		superseded := answer["supersededPerTurn"].(map[string]any)
		if answer["events"] != 3.0 || answer["duplicateInvocations"] != 3.0 || answer["turnsWithMoreThanOneEvent"] != 1.0 || superseded["pairs"] != 1.0 || superseded["pairsWithMoreThanOneRow"] != 1.0 {
			t.Fatalf("counters: %v", answer)
		}
	})
	t.Run("one root spelled twice is read once", func(t *testing.T) {
		h, _ := oneEvent(t)
		alias := h.root + "/alias"
		if err := os.Symlink(h.journal, alias); err != nil {
			t.Fatal(err)
		}
		code, answer := verify(t, roots(h.journal, alias)...)
		expectVerdict(t, code, answer, 0, "TRUE")
		if state := listed(answer, "roots")[1].(map[string]any)["state"]; state != "same_root_as_another_spelling" {
			t.Fatalf("the alias was read as %v", state)
		}
	})
	t.Run("a fault after the guard's answer is still one event accepted once", func(t *testing.T) {
		h, records := oneEvent(t)
		fault := func(o hook.Object) hook.Object {
			return set("held", false)(set("adapterOutcome", hook.AdapterFaulted)(o))
		}
		change(t, records[hook.Accepted], func(o hook.Object) hook.Object { return set("fault", "OSError: injected")(fault(o)) })
		change(t, records["outcome"], fault)
		code, answer := verify(t, roots(h.journal)...)
		expectVerdict(t, code, answer, 0, "TRUE")
	})
	t.Run("every record rewritten in its writer's own bytes", func(t *testing.T) {
		h, records := oneEvent(t)
		for _, path := range records {
			change(t, path, func(o hook.Object) hook.Object { return o })
		}
		code, answer := verify(t, roots(h.journal)...)
		expectVerdict(t, code, answer, 0, "TRUE")
	})
	// A Codex home and journal root under a directory whose name is not UTF-8, as the Python
	// adapter records them: the claim's host ledger and the host file's journal root name the
	// byte as its surrogate escape (\udcff), which is that byte again when the path is handed to
	// the system (os.fsencode). The ledger the claim names is found and read once, whichever
	// spellings name it, and is named as the claim spells it.
	t.Run("a Codex home and journal root whose names are not UTF-8", func(t *testing.T) {
		for _, c := range []struct{ name, spelled string }{
			{"b-\xff", "b-\\udcff"},
			{"b-\xe9x", "b-\\udce9x"},
			{"b-\xed\xb3\xbf", "b-\\udced\\udcb3\\udcbf"},
		} {
			h, records := oneEvent(t)
			dir := h.root + "/" + c.name
			codex, journal := dir+"/codex", dir+"/journal"
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			for from, to := range map[string]string{h.codex: codex, h.journal: journal} {
				if err := os.Rename(from, to); err != nil {
					t.Fatal(err)
				}
			}
			ledger := codex + "/" + strings.Join(hook.HostLedgerParts, "/")
			change(t, journal+strings.TrimPrefix(records["claim"], h.journal), set("claimedBy.hostLedger", store.FSDecode(ledger)))
			change(t, ledger+"/"+filepath.Base(records["host"]), set("claimedBy.journalRoot", store.FSDecode(journal)))
			for _, args := range [][]string{roots(journal), append(roots(journal), "--codex-home", codex)} {
				var out, errs bytes.Buffer
				code := Run(args, &out, &errs)
				var answer map[string]any
				if err := json.Unmarshal(out.Bytes(), &answer); err != nil {
					t.Fatal(err, errs.String())
				}
				expectVerdict(t, code, answer, 0, "TRUE")
				ledgers := listed(answer, "hostLedgers")
				if len(ledgers) != 1 || ledgers[0].(map[string]any)["state"] != "read" || !strings.Contains(out.String(), `"ledger": "`+h.root+"/"+c.spelled+`/codex/`) {
					t.Fatalf("%q %v: %s", c.name, args, out.String())
				}
			}
		}
	})
	// Python keys a host ledger by the str that names it, so a claim spelling a name's bytes as
	// surrogate escapes where they are UTF-8 (\udcc3\udca9 for \u00e9) names the ledger it reaches
	// under its own spelling: read once, and beside the Codex home's own spelling the same ledger
	// reached again.
	t.Run("a claim that spells its ledger otherwise than the system does", func(t *testing.T) {
		h, records := oneEvent(t)
		dir := h.root + "/b-\u00e9"
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(h.codex, dir+"/codex"); err != nil {
			t.Fatal(err)
		}
		ledger := "/codex/" + strings.Join(hook.HostLedgerParts, "/")
		change(t, records["claim"], set("claimedBy.hostLedger", h.root+"/b-\xed\xb3\x83\xed\xb2\xa9"+ledger))
		for _, c := range []struct {
			args   []string
			states []string
		}{
			{roots(h.journal), []string{"read"}},
			{append(roots(h.journal), "--codex-home", dir+"/codex"), []string{"read", "same_ledger_as_another_spelling"}},
		} {
			var out, errs bytes.Buffer
			code := Run(c.args, &out, &errs)
			var answer map[string]any
			if err := json.Unmarshal(out.Bytes(), &answer); err != nil {
				t.Fatal(err, errs.String())
			}
			expectVerdict(t, code, answer, 0, "TRUE")
			ledgers := listed(answer, "hostLedgers")
			if len(ledgers) != len(c.states) || ledgers[len(ledgers)-1].(map[string]any)["state"] != c.states[len(c.states)-1] || !strings.Contains(out.String(), `"ledger": "`+h.root+`/b-\udcc3\udca9`+ledger+`"`) {
				t.Fatalf("%v: %s", c.args, out.String())
			}
		}
	})
}

// SEV-6: FALSE when an event was accepted more than once.
func TestSEV06_AnEventAcceptedTwiceReadsFalse(t *testing.T) {
	t.Run("in two journal roots, seen only when both are read", func(t *testing.T) {
		first, second := newHost(t, hook.Release), newHost(t, hook.Release)
		payload := first.at(loadFixture(t), 0)
		first.run(first.settings, payload)
		second.run(second.settings, payload)
		for _, h := range []*host{first, second} {
			code, answer := verify(t, roots(h.journal)...)
			expectVerdict(t, code, answer, 0, "TRUE")
		}
		code, answer := verify(t, roots(first.journal, second.journal)...)
		expectVerdict(t, code, answer, 1, "FALSE")
		if len(listed(answer, "eventsWithMoreThanOneAcceptance")) != 1 {
			t.Fatal(answer)
		}
	})
	t.Run("an accepted row whose claim is gone", func(t *testing.T) {
		h, records := oneEvent(t)
		if err := os.Remove(records["claim"]); err != nil {
			t.Fatal(err)
		}
		code, answer := verify(t, roots(h.journal)...)
		expectVerdict(t, code, answer, 1, "FALSE")
		if len(listed(answer, "acceptedRowsWithoutLedger")) != 1 {
			t.Fatal(answer)
		}
	})
	t.Run("a duplicate that asked the guard anyway", func(t *testing.T) {
		h, records := oneEvent(t)
		change(t, records[hook.Duplicate], set("guardInvoked", true))
		code, answer := verify(t, roots(h.journal)...)
		expectVerdict(t, code, answer, 1, "FALSE")
		if len(listed(answer, "guardAskedOnDuplicate")) != 1 {
			t.Fatal(answer)
		}
	})
}

// SEV-7: an invocation the reading cannot judge keeps its window from TRUE.
func TestSEV07_UnjudgedInvocationsKeepTheWindowFromTrue(t *testing.T) {
	loose := func(t *testing.T, policy string, runs int) map[string]any {
		h := newHost(t, hook.Release)
		h.writeSettings(h.settings, func(d map[string]any) { d["journalPolicy"] = policy })
		h.run(h.settings, h.at(loadFixture(t), 0))
		for range runs {
			h.run(h.settings, looseStop)
		}
		code, answer := verify(t, roots(h.journal)...)
		if code == 0 {
			t.Fatalf("%s: unjudged invocations read TRUE: %v", policy, answer)
		}
		return answer
	}
	t.Run("an owner that died before answering", func(t *testing.T) {
		h := newHost(t, hook.Release)
		h.dieFirst = true
		payload := h.at(loadFixture(t), 0)
		h.twice(payload)
		if h.calls.Load() != 1 {
			t.Fatalf("the guard was asked %d times", h.calls.Load())
		}
		code, answer := verify(t, roots(h.journal)...)
		expectVerdict(t, code, answer, 3, "UNREADABLE")
		if len(listed(answer, "acceptedWithoutOutcome")) != 1 || len(listed(answer, "eventsWithMoreThanOneAcceptance")) != 0 {
			t.Fatal(answer)
		}
	})
	t.Run("payloads without a transcript path", func(t *testing.T) {
		answer := loose(t, hook.EveryInvocation, 2)
		if got := answer["unjudgedInvocations"].(map[string]any); len(got) != 1 || got["unestablished:transcript_path_missing"] != 2.0 {
			t.Fatal(answer)
		}
	})
	t.Run("under faults_only they stay visible", func(t *testing.T) {
		answer := loose(t, hook.FaultsOnly, 1)
		if got := answer["unjudgedInvocations"].(map[string]any); got["unestablished:transcript_path_missing"] != 1.0 {
			t.Fatal(answer)
		}
	})
	t.Run("under no_journal nothing is seen, so nothing is vouched for", func(t *testing.T) {
		loose(t, hook.NoJournal, 1)
	})
	t.Run("a Stop that cannot be told from a late delivery of the one before", func(t *testing.T) {
		h := newHost(t, hook.Block)
		document := loadFixture(t)
		for _, index := range []int{0, 2, 4} {
			h.twice(h.at(document, index))
		}
		code, answer := verify(t, roots(h.journal)...)
		expectVerdict(t, code, answer, 3, "UNREADABLE")
		if got := answer["unjudgedInvocations"].(map[string]any); len(got) != 1 || got["unestablished:answer_text_ambiguous"] != 2.0 || answer["events"] != 2.0 {
			t.Fatal(answer)
		}
	})
	// Decision 22 over a relay state directory whose name is not UTF-8, or holds a character
	// CPython 3.14's Unicode database leaves unassigned: the failed dial's row spells the socket
	// as repr() does, the byte as its surrogate escape and the character escaped, and that row is
	// still the exempt one.
	t.Run("a failed native dial under a state directory named as Python names it", func(t *testing.T) {
		for _, c := range []struct{ name, spelled string }{
			{"st\xffate", `st\\udcffate/control.sock`},
			{"st\xed\xa0\x80ate", `st\\udced\\udca0\\udc80ate/control.sock`},
			{"st\u0c5cate", `st\\u0c5cate/control.sock`},
		} {
			h := newHost(t, hook.Release)
			if err := os.Remove(h.state + "/control.sock"); err != nil {
				t.Fatal(err)
			}
			h.state = h.root + "/" + c.name
			if err := os.Mkdir(h.state, 0o700); err != nil {
				t.Fatal(err)
			}
			h.run(h.settings, h.at(loadFixture(t), 0))
			rows := h.rowPaths(h.journal)
			if len(rows) != 1 || valueAt(t, rows[0], "adapterOutcome") != hook.GuardUnreachable {
				t.Fatalf("%q: not one failed native dial: %v", c.name, rows)
			}
			if raw, _ := os.ReadFile(rows[0]); !bytes.Contains(raw, []byte(c.spelled+"'"+`"`)) {
				t.Fatalf("%q: the row does not spell the socket as repr() does: %s", c.name, raw)
			}
			code, answer := verify(t, roots(h.journal)...)
			expectVerdict(t, code, answer, 0, "TRUE")
			if got := answer["unjudgedInvocations"].(map[string]any); len(got) != 1 || got["no_event:guard_unreachable"] != 1.0 || len(listed(answer, "rowsUnreadable")) != 0 {
				t.Fatalf("%q: %v", c.name, answer)
			}
		}
	})
	t.Run("rows from before event identity", func(t *testing.T) {
		h, _ := oneEvent(t)
		legacy := h.journal + "/20000101/" + strings.Repeat("0", 31) + "7.json"
		if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
			t.Fatal(err)
		}
		row := `{"at": "2000-01-01T00:00:00Z", "recordVersion": 1, "sessionId": "s", "stopHookActive": false, "turnId": "t"}` + "\n"
		if err := os.WriteFile(legacy, []byte(row), 0o600); err != nil {
			t.Fatal(err)
		}
		code, answer := verify(t, roots(h.journal)...)
		expectVerdict(t, code, answer, 3, "UNREADABLE")
		if answer["legacyRows"] != 1.0 {
			t.Fatal(answer)
		}
		code, answer = verify(t, append(roots(h.journal), "--since", "2001-01-01T00:00:00Z")...)
		expectVerdict(t, code, answer, 0, "TRUE")
	})
	t.Run("a duplicate whose accepted record is in no root read", func(t *testing.T) {
		h := newHost(t, hook.Release)
		other, settings := h.ownRoot("other-journal")
		payload := h.at(loadFixture(t), 0)
		h.run(h.settings, payload)
		h.run(settings, payload)
		code, answer := verify(t, roots(other)...)
		expectVerdict(t, code, answer, 3, "UNREADABLE")
		if len(listed(answer, "duplicatesWithoutClaim")) != 1 {
			t.Fatal(answer)
		}
		code, answer = verify(t, roots(h.journal, other)...)
		expectVerdict(t, code, answer, 0, "TRUE")
	})
}

// SEV-8: the ledger is read whole; what it cannot vouch for is named.
func TestSEV08_LedgerIntegrity(t *testing.T) {
	named := func(t *testing.T, answer map[string]any, list string, want ...string) {
		t.Helper()
		got := listed(answer, list)
		if len(got) != len(want) {
			t.Fatalf("%s = %v, want %v", list, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s = %v, want %v", list, got, want)
			}
		}
	}
	unreadable := func(t *testing.T, h *host) map[string]any {
		t.Helper()
		code, answer := verify(t, roots(h.journal)...)
		expectVerdict(t, code, answer, 3, "UNREADABLE")
		return answer
	}
	t.Run("a torn claim", func(t *testing.T) {
		h, records := oneEvent(t)
		if err := os.WriteFile(records["claim"], nil, 0o600); err != nil {
			t.Fatal(err)
		}
		named(t, unreadable(t, h), "ledgerUnreadable", records["claim"])
	})
	t.Run("a claim whose answer is not its key's", func(t *testing.T) {
		h, records := oneEvent(t)
		change(t, records["claim"], set("answerItem", "msg_another_answer"))
		named(t, unreadable(t, h), "ledgerUnreadable", records["claim"])
	})
	t.Run("an outcome with no claim, whatever the window", func(t *testing.T) {
		h, _ := oneEvent(t)
		key := strings.Repeat("e", 64)
		orphan := `{"adapterOutcome": "guard_answered", "at": "2026-09-23T00:00:00Z", "attemptRow": null, "eventKey": "` + key + `", "guardDecision": "release", "guardState": "unmanaged", "held": false, "journalPolicy": "faults_only", "ledgerVersion": 1, "sessionId": "s", "turnId": "t"}` + "\n"
		if err := os.WriteFile(h.journal+"/accepted/"+key+hook.OutcomeSuffix, []byte(orphan), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, window := range [][]string{nil, {"--turn", "t"}, {"--since", "2000-01-01T00:00:00Z"}} {
			code, answer := verify(t, append(roots(h.journal), window...)...)
			if code == 0 {
				t.Fatalf("%v vouched for an orphaned outcome", window)
			}
			named(t, answer, "outcomesWithoutClaim", key)
		}
	})
	t.Run("an outcome in another root does not complete a claim", func(t *testing.T) {
		first, second := newHost(t, hook.Release), newHost(t, hook.Release)
		first.run(first.settings, first.at(loadFixture(t), 0))
		records := first.records()
		if err := os.MkdirAll(second.journal+"/accepted", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(records["outcome"], second.journal+"/accepted/"+filepath.Base(records["outcome"])); err != nil {
			t.Fatal(err)
		}
		if code, answer := verify(t, roots(first.journal, second.journal)...); code == 0 {
			t.Fatal(answer)
		}
	})
	for _, c := range []struct {
		name string
		edit func(hook.Object) hook.Object
	}{
		{"an outcome naming another session", set("sessionId", "another-session")},
		{"an outcome without its time", pop("at")},
		{"a claim of another ledger version", set("ledgerVersion", int64(9))},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, records := oneEvent(t)
			target := records["outcome"]
			if strings.HasPrefix(c.name, "a claim") {
				target = records["claim"]
			}
			change(t, target, c.edit)
			unreadable(t, h)
		})
	}
	t.Run("an outcome whose accepted row is gone", func(t *testing.T) {
		h, records := oneEvent(t)
		if err := os.Remove(records[hook.Accepted]); err != nil {
			t.Fatal(err)
		}
		named(t, unreadable(t, h), "acceptedRowsMissing", strings.TrimSuffix(filepath.Base(records["claim"]), ".json"))
	})
	t.Run("an outcome outside the window is still checked against its claim", func(t *testing.T) {
		h, records := oneEvent(t)
		change(t, records["outcome"], func(o hook.Object) hook.Object {
			return set("sessionId", "wrong-session")(set("at", "2000-01-01T00:00:00Z")(o))
		})
		code, answer := verify(t, append(roots(h.journal), "--since", "2020-01-01T00:00:00Z")...)
		expectVerdict(t, code, answer, 3, "UNREADABLE")
		named(t, answer, "ledgerUnreadable", records["outcome"])
	})
	t.Run("a ledger path that is not a directory is not an empty ledger", func(t *testing.T) {
		good, _ := oneEvent(t)
		broken := good.root + "/broken"
		if err := os.MkdirAll(broken, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(broken+"/accepted", []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		code, answer := verify(t, roots(good.journal, broken)...)
		expectVerdict(t, code, answer, 3, "UNREADABLE")
		if state := listed(answer, "roots")[1].(map[string]any); state["state"] != "unreadable" || state["detail"] != broken+"/accepted is not a directory" {
			t.Fatal(state)
		}
	})
	// A root that cannot be examined is named as str(OSError) names it: repr() of the path as
	// Python holds it, so a character str.isprintable refuses is escaped and a byte that is not
	// UTF-8 is the surrogate surrogateescape made of it. A root named in a message of the
	// reader's own carries the path as str() holds it, which json.dumps writes as \udcXX.
	t.Run("a root that cannot be examined is named as Python names it", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root is never refused a directory's search permission")
		}
		base, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		locked, linked := base+"/locked", base+"/r\xffx"
		for _, dir := range []string{locked, linked} {
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink(base, linked+"/accepted"); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
		var args []string
		for _, name := range []string{"a\u00a0b", "a\u2028b", "a\xffb", "a\xed\xa0\x80b"} {
			args = append(args, "--journal-root", locked+"/"+name)
		}
		var out, errs bytes.Buffer
		if code := Run(append(args, "--journal-root", linked), &out, &errs); code != 3 {
			t.Fatalf("exit %d: %s", code, errs.String())
		}
		for _, line := range []string{
			`"detail": "[Errno 13] Permission denied: '` + locked + `/a\\xa0b'",`,
			`"detail": "[Errno 13] Permission denied: '` + locked + `/a\\u2028b'",`,
			`"detail": "[Errno 13] Permission denied: '` + locked + `/a\\udcffb'",`,
			`"detail": "[Errno 13] Permission denied: '` + locked + `/a\\udced\\udca0\\udc80b'",`,
			`"root": "` + locked + `/a\udced\udca0\udc80b",`,
			`"detail": "` + base + `/r\udcffx/accepted is a link, which the adapter never makes",`,
			`"root": "` + base + `/r\udcffx",`,
		} {
			if !strings.Contains(out.String(), line) {
				t.Fatalf("no %s in\n%s", line, out.String())
			}
		}
	})
	// A root or Codex home is where os.path.abspath(Path(...).expanduser()) puts it: a ~ is HOME
	// when HOME is set at all, the root when it is empty, and this user's passwd entry when it is
	// unset; a relative path is joined to the directory the kernel reports, not $PWD's spelling of
	// it through a link; and a working directory that is gone is the FileNotFoundError getcwd
	// raises, the reader's fault.
	t.Run("a root is read where Python reads it", func(t *testing.T) {
		me, err := user.Current()
		if err != nil || me.HomeDir == "" {
			t.Skip("no passwd entry names this user's home")
		}
		base, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Chdir(base)
		reading := func(home *string, args ...string) map[string]any {
			t.Helper()
			if home == nil {
				t.Setenv("HOME", "")
				if err := os.Unsetenv("HOME"); err != nil {
					t.Fatal(err)
				}
			} else {
				t.Setenv("HOME", *home)
			}
			var out, errs bytes.Buffer
			Run(args, &out, &errs)
			var answer map[string]any
			if err := json.Unmarshal(out.Bytes(), &answer); err != nil {
				t.Fatal(err, errs.String())
			}
			return answer
		}
		empty, tilde := "", "~x"
		missing := "nonexistent-" + filepath.Base(filepath.Dir(base))
		described := func(home *string) string {
			if home == nil {
				return " unset"
			}
			return "=" + *home
		}
		for _, c := range []struct {
			home       *string
			args       []string
			root, host string
		}{
			{&empty, roots("~"), "/", ""},
			{&empty, roots("~/"), "/", ""},
			{&empty, append(roots("/"+missing), "--codex-home", "~"), "/" + missing, "/crw-completion-hook/stop-events"},
			{nil, append(roots("~/"+missing), "--codex-home", "~/"+missing), me.HomeDir + "/" + missing, me.HomeDir + "/" + missing + "/crw-completion-hook/stop-events"},
		} {
			answer := reading(c.home, c.args...)
			if got := listed(answer, "roots")[0].(map[string]any)["root"]; got != c.root {
				t.Errorf("HOME%s %v: root %v, want %s", described(c.home), c.args, got, c.root)
			}
			if ledgers := listed(answer, "hostLedgers"); c.host != "" && (len(ledgers) != 1 || ledgers[0].(map[string]any)["ledger"] != c.host) {
				t.Errorf("HOME%s %v: host ledgers %v, want %s", described(c.home), c.args, ledgers, c.host)
			}
		}
		for _, args := range [][]string{roots("~"), append(roots("/"+missing), "--codex-home", "~")} {
			answer := reading(&tilde, args...)
			if answer["readerFault"] != "RuntimeError: Could not determine home directory." || answer["verdict"] != "UNREADABLE" {
				t.Errorf("HOME=~x %v: readerFault %v, verdict %v", args, answer["readerFault"], answer["verdict"])
			}
		}
		if err := os.Mkdir(base+"/real", 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("real", base+"/link"); err != nil {
			t.Fatal(err)
		}
		t.Chdir(base + "/link")
		if got := listed(reading(&base, roots("j")...), "roots")[0].(map[string]any)["root"]; got != base+"/real/j" {
			t.Errorf("a relative root under a linked working directory is %v", got)
		}
		if err := os.Mkdir(base+"/gone", 0o700); err != nil {
			t.Fatal(err)
		}
		t.Chdir(base + "/gone")
		if err := os.Remove(base + "/gone"); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{roots("rel"), append(roots("/"+missing), "--codex-home", "rel")} {
			answer := reading(&base, args...)
			if answer["readerFault"] != "FileNotFoundError: [Errno 2] No such file or directory" || answer["verdict"] != "UNREADABLE" {
				t.Errorf("%v under a removed working directory: readerFault %v, verdict %v", args, answer["readerFault"], answer["verdict"])
			}
		}
	})
	t.Run("an entry in the ledger this reader does not know", func(t *testing.T) {
		h, _ := oneEvent(t)
		stray := h.journal + "/accepted/pending.json.tmp"
		if err := os.WriteFile(stray, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		named(t, unreadable(t, h), "foreignLedgerEntries", stray)
	})
	t.Run("a host file whose owner died before its claim", func(t *testing.T) {
		h, records := oneEvent(t)
		for _, kind := range []string{"claim", "outcome", hook.Accepted, hook.Duplicate} {
			if err := os.Remove(records[kind]); err != nil {
				t.Fatal(err)
			}
		}
		// The first event is gone but for its host file; a second, complete one beside it.
		h.twice(h.at(distinctThird(t), 2))
		named(t, unreadable(t, h), "hostFilesWithoutClaim", records["host"])
	})
	t.Run("a claim whose host file is gone", func(t *testing.T) {
		h, records := oneEvent(t)
		if err := os.Remove(records["host"]); err != nil {
			t.Fatal(err)
		}
		named(t, unreadable(t, h), "claimsWithoutHostFile", records["claim"])
	})
}

// SEV-9: a row the reader cannot classify is named, never skipped, whatever the window.
func TestSEV09_RowIntegrity(t *testing.T) {
	t.Run("a version-2 row it cannot classify", func(t *testing.T) {
		h, records := oneEvent(t)
		stray := filepath.Dir(records[hook.Accepted]) + "/" + strings.Repeat("d", 32) + ".json"
		if err := os.WriteFile(stray, []byte(`{"acceptance": "accepted", "recordVersion": 2}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		session := loadFixture(t).Stops[0].Payload["session_id"].(string)
		for _, window := range [][]string{nil, {"--since", "2000-01-01T00:00:00Z"}, {"--session", session}} {
			code, answer := verify(t, append(roots(h.journal), window...)...)
			expectVerdict(t, code, answer, 3, "UNREADABLE")
			if got := listed(answer, "rowsUnreadable"); len(got) != 1 || got[0] != stray {
				t.Fatalf("%v: %v", window, got)
			}
		}
	})
	for _, c := range []struct {
		name, kind string
		edit       func(hook.Object) hook.Object
	}{
		{"a row version this reader does not know", hook.Accepted, set("recordVersion", int64(3))},
		{"an accepted row about another session", hook.Accepted, set("sessionId", "01a0cd4a-0000-7000-8000-000000000000")},
		{"a duplicate row about another turn", hook.Duplicate, set("turnId", "01a0cd4a-0000-7000-8000-000000000001")},
		{"an accepted row without its invocation fields", hook.Accepted, func(o hook.Object) hook.Object {
			return pop("acceptedAs")(pop("adapterOutcome")(pop("guardInvoked")(o)))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, records := oneEvent(t)
			change(t, records[c.kind], c.edit)
			code, answer := verify(t, roots(h.journal)...)
			expectVerdict(t, code, answer, 3, "UNREADABLE")
			if got := listed(answer, "rowsUnreadable"); len(got) != 1 || got[0] != records[c.kind] {
				t.Fatal(got)
			}
		})
	}
	t.Run("a row the reader cannot finish reading is a reader fault, never TRUE", func(t *testing.T) {
		h, _ := oneEvent(t)
		legacy := h.journal + "/20000101/" + strings.Repeat("0", 32) + ".json"
		if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(legacy, []byte(`{"at": "2026-09-23T00:00:00Z", "recordVersion": 1, "sessionId": ["s"], "turnId": "t"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		code, answer := verify(t, roots(h.journal)...)
		expectVerdict(t, code, answer, 3, "UNREADABLE")
		if answer["readerFault"] != "TypeError: cannot use 'tuple' as a dict key (unhashable type: 'list')" {
			t.Fatal(answer["readerFault"])
		}
	})
}

// SEV-10: the records of one event are written by one owner in one run, so they agree.
func TestSEV10_RecordsOfOneEventAgree(t *testing.T) {
	slot := "20000101/" + strings.Repeat("0", 32) + ".json"
	for _, c := range []struct {
		name, kind string
		edit       func(hook.Object) hook.Object
	}{
		{"claim names another slot", "claim", set("claimedBy.attemptRow", slot)},
		{"host file names another slot", "host", set("claimedBy.attemptRow", slot)},
		{"host file names another owner", "host", set("claimedBy.pid", int64(1))},
		{"outcome names another slot", "outcome", set("attemptRow", slot)},
		{"outcome held what the row released", "outcome", set("held", true)},
		{"outcome decided otherwise", "outcome", set("guardDecision", hook.Block)},
		{"outcome saw another state", "outcome", set("guardState", "declared")},
		{"outcome ended otherwise", "outcome", set("adapterOutcome", "guard_timed_out")},
		{"accepted row calls itself a duplicate", hook.Accepted, set("adapterOutcome", hook.DuplicateInvocation)},
		{"accepted row held without an answer", hook.Accepted, func(o hook.Object) hook.Object {
			return set("adapterOutcome", "guard_timed_out")(set("held", true)(o))
		}},
		{"duplicate row printed a hold", hook.Duplicate, set("held", true)},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, records := oneEvent(t)
			change(t, records[c.kind], c.edit)
			code, answer := verify(t, roots(h.journal)...)
			expectVerdict(t, code, answer, 3, "UNREADABLE")
		})
	}
	t.Run("a duplicate naming a claim its own root does not hold", func(t *testing.T) {
		h := newHost(t, hook.Release)
		other, settings := h.ownRoot("other-journal")
		payload := h.at(loadFixture(t), 0)
		h.run(h.settings, payload)
		h.run(settings, payload)
		duplicate := h.rowPaths(other)[0]
		change(t, duplicate, func(o hook.Object) hook.Object {
			return set("acceptedAs", "accepted/"+readObject(t, duplicate)["eventKey"].(string)+".json")(o)
		})
		code, answer := verify(t, roots(h.journal, other)...)
		expectVerdict(t, code, answer, 3, "UNREADABLE")
		if got := listed(answer, "recordsThatDisagree"); len(got) != 1 || got[0] != duplicate {
			t.Fatal(got)
		}
	})
}

// SEV-11: only what the adapter can write is vouched for, and a missing field is read, not a fault.
func TestSEV11_OnlyWhatTheWriterWritesIsVouchedFor(t *testing.T) {
	both := []string{hook.Accepted, "outcome"}
	for _, c := range []struct {
		name  string
		kinds []string
		edit  func(hook.Object) hook.Object
	}{
		{"an outcome the runtime cannot write", both, set("adapterOutcome", "invented")},
		{"a decision the runtime cannot write", both, set("guardDecision", "nonsense")},
		{"a release that held", both, set("held", true)},
		{"a block that did not hold", both, set("guardDecision", hook.Block)},
		{"an answer under faults_only beside its row", []string{"outcome"}, set("journalPolicy", hook.FaultsOnly)},
		{"a duplicate carrying a decision", []string{hook.Duplicate}, set("guardDecision", hook.Block)},
		{"a duplicate carrying a process ending", []string{hook.Duplicate}, set("processEnding", hook.Exited)},
		{"a row that asked nothing carrying a receipt", []string{hook.Duplicate}, set("assignmentId", "receipt")},
		{"a guard outcome that does not follow from its call", []string{hook.Accepted}, set("stdoutReading", hook.SaidNothing)},
		{"a signal from a process that exited", []string{hook.Accepted}, set("signal", int64(9))},
		{"an outcome missing its row's slot", []string{"outcome"}, pop("attemptRow")},
		{"a host file missing its owner's root", []string{"host"}, pop("claimedBy.journalRoot")},
		{"a duplicate missing its decision", []string{hook.Duplicate}, pop("guardDecision")},
		{"an accepted row missing its settings path", []string{hook.Accepted}, pop("configuration")},
		{"a fault not asked, yet called and answered", []string{hook.Accepted}, func(o hook.Object) hook.Object {
			return set("guardInvoked", false)(set("fault", "OSError: injected")(set("held", false)(set("adapterOutcome", hook.AdapterFaulted)(o))))
		}},
		{"a fault answered without a call", []string{hook.Accepted}, func(o hook.Object) hook.Object {
			return set("processEnding", nil)(set("stdoutReading", nil)(set("exitCode", nil)(set("fault", "OSError: injected")(set("adapterOutcome", hook.AdapterFaulted)(o)))))
		}},
		{"a version of another JSON type", []string{hook.Accepted}, set("recordVersion", 2.0)},
		{"a ledger version that is a bool", []string{"host"}, set("ledgerVersion", true)},
		{"an impossible time", []string{"claim"}, set("claimedAt", "2026-99-99T99:99:99Z")},
		{"a mode no settings give", []string{hook.Duplicate}, set("guardMode", nil)},
		{"a relative transcript path", []string{hook.Accepted}, set("eventIdentity.transcriptPath", "rollout.jsonl")},
		{"a relative settings path", []string{hook.Accepted}, set("configuration", "crw-completion-hook.json")},
		{"a detail on an answered call", []string{hook.Accepted}, set("detail", "the guard did not answer")},
		{"a field the writer never puts there", []string{hook.Accepted}, set("journalledAs", nil)},
		{"a field its writer never puts in a claim", []string{"claim"}, set("claimedBy.journalRoot", nil)},
		{"a field its writer never puts in a host file", []string{"host"}, set("claimedBy.hostLedger", "/x")},
		{"a duplicate saying something else", []string{hook.Duplicate}, set("detail", "the guard was not asked")},
		{"stderr longer than is kept", []string{hook.Accepted}, set("guardStderr", strings.Repeat("x", hook.StderrLimit+1))},
		{"a fault on a duplicate, which releases before anything can fault", []string{hook.Duplicate}, func(o hook.Object) hook.Object {
			return set("fault", "OSError: injected")(set("adapterOutcome", hook.AdapterFaulted)(o))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, records := oneEvent(t)
			for _, kind := range c.kinds {
				change(t, records[kind], c.edit)
			}
			code, answer := verify(t, append(roots(h.journal), "--codex-home", h.codex)...)
			expectVerdict(t, code, answer, 3, "UNREADABLE")
			if _, fault := answer["readerFault"]; fault {
				t.Fatalf("the reader stopped instead of reading: %v", answer["readerFault"])
			}
		})
	}
	t.Run("a row under a day that is no date", func(t *testing.T) {
		h, records := oneEvent(t)
		impossible := h.journal + "/99999999/" + filepath.Base(records[hook.Duplicate])
		raw, _ := os.ReadFile(records[hook.Duplicate])
		if err := os.MkdirAll(filepath.Dir(impossible), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(impossible, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		code, answer := verify(t, roots(h.journal)...)
		expectVerdict(t, code, answer, 3, "UNREADABLE")
		if got := listed(answer, "rowsUnreadable"); len(got) != 1 || got[0] != filepath.Dir(impossible) {
			t.Fatal(got)
		}
	})
	// A native dial that failed before the identity scan is judged by its row alone (decision 22),
	// and its timings are completion._is_count: a Python int of any size. A count past int64 is
	// still a count, so it keeps the window TRUE exactly as it did under Python; a negative one
	// of the same width is not a count at all.
	t.Run("a native dial's timings are counts of any size", func(t *testing.T) {
		for _, c := range []struct {
			field, value string
			want         int
		}{
			{"elapsedMs", "9223372036854775807", 0},
			{"elapsedMs", "9223372036854775808", 0},
			{"elapsedMs", "18446744073709551616", 0},
			{"elapsedMs", "1" + strings.Repeat("0", 30), 0},
			{"guardElapsedMs", "9223372036854775808", 0},
			{"guardElapsedMs", "1" + strings.Repeat("0", 30), 0},
			{"elapsedMs", "-9223372036854775809", 3},
			{"guardElapsedMs", "-1", 3},
		} {
			h := newHost(t, hook.Release)
			if err := os.Remove(h.state + "/control.sock"); err != nil {
				t.Fatal(err)
			}
			h.run(h.settings, h.at(loadFixture(t), 0))
			rows := h.rowPaths(h.journal)
			if len(rows) != 1 || valueAt(t, rows[0], "adapterOutcome") != hook.GuardUnreachable {
				t.Fatalf("not one failed native dial: %v", rows)
			}
			change(t, rows[0], set(c.field, json.Number(c.value)))
			if raw, _ := os.ReadFile(rows[0]); !bytes.Contains(raw, []byte(`"`+c.field+`": `+c.value+`,`)) {
				t.Fatalf("the row does not carry %s %s: %s", c.field, c.value, raw)
			}
			code, answer := verify(t, roots(h.journal)...)
			verdict := map[int]string{0: "TRUE", 3: "UNREADABLE"}[c.want]
			expectVerdict(t, code, answer, c.want, verdict)
			if unread := len(listed(answer, "rowsUnreadable")); (c.want == 0) != (unread == 0) {
				t.Fatalf("%s %s: rowsUnreadable %v", c.field, c.value, answer["rowsUnreadable"])
			}
		}
	})
	t.Run("an unestablished row carries only what its reason had recorded", func(t *testing.T) {
		for _, c := range []struct {
			reason       string
			path, item   any
			countedAsOne bool
		}{
			{"made_up", nil, nil, false},
			{"transcript_path_missing", "/x/rollout.jsonl", nil, false},
			{"transcript_absent", nil, nil, false},
			{"transcript_absent", "/x/rollout.jsonl", "m", false},
			{"transcript_path_relative", "/x/rollout.jsonl", nil, false},
			{"session_mismatch", "/x/rollout.jsonl", nil, false},
			{"transcript_absent", "/x/rollout.jsonl", nil, true},
			{"session_mismatch", "/x/rollout.jsonl", "m", true},
		} {
			h := newHost(t, hook.Release)
			h.run(h.settings, looseStop)
			change(t, h.rowPaths(h.journal)[0], func(o hook.Object) hook.Object {
				return set("eventIdentity.answerItem", c.item)(set("eventIdentity.transcriptPath", c.path)(set("eventIdentity.reason", c.reason)(o)))
			})
			code, answer := verify(t, roots(h.journal)...)
			expectVerdict(t, code, answer, 3, "UNREADABLE")
			counted := answer["unjudgedInvocations"].(map[string]any)
			if c.countedAsOne != (len(counted) == 1 && counted["unestablished:"+c.reason] == 1.0) || (!c.countedAsOne && len(counted) != 0) {
				t.Fatalf("%+v: counted %v", c, counted)
			}
		}
	})
}

// SEV-12: a record is what the writer's create-once produced, where it put it, in its bytes.
func TestSEV12_OnDiskFormIsTheWriters(t *testing.T) {
	kinds := []string{hook.Accepted, hook.Duplicate, "claim", "outcome", "host"}
	unreadable := func(t *testing.T, h *host) map[string]any {
		t.Helper()
		code, answer := verify(t, roots(h.journal)...)
		expectVerdict(t, code, answer, 3, "UNREADABLE")
		return answer
	}
	for _, kind := range kinds {
		t.Run("a "+kind+" replaced by a link", func(t *testing.T) {
			h, records := oneEvent(t)
			elsewhere := h.root + "/elsewhere.json"
			if err := os.Rename(records[kind], elsewhere); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(elsewhere, records[kind]); err != nil {
				t.Fatal(err)
			}
			unreadable(t, h)
		})
		forms := map[string]func([]byte) []byte{
			"indented":   func(raw []byte) []byte { return bytes.ReplaceAll(raw, []byte(", "), []byte(",\n ")) },
			"no newline": func(raw []byte) []byte { return bytes.TrimSuffix(raw, []byte("\n")) },
			"duplicate key": func(raw []byte) []byte {
				first := raw[2 : 2+bytes.IndexByte(raw[2:], '"')]
				return append([]byte(`{"`+string(first)+`": "decoy", `), raw[1:]...)
			},
			"unsorted": func(raw []byte) []byte {
				body, _ := hook.Decode(raw)
				o := body.(hook.Object)
				for i, j := 0, len(o)-1; i < j; i, j = i+1, j-1 {
					o[i], o[j] = o[j], o[i]
				}
				return []byte(evidenceDumps(o) + "\n")
			},
		}
		for name, form := range forms {
			t.Run("a "+kind+" in "+name+" bytes", func(t *testing.T) {
				h, records := oneEvent(t)
				raw, _ := os.ReadFile(records[kind])
				if err := os.WriteFile(records[kind], form(raw), 0o600); err != nil {
					t.Fatal(err)
				}
				unreadable(t, h)
			})
		}
	}
	// A field the reader never types can hold anything its writer's JSON can, as deep as
	// CPython 3.14's json nests: past encoding/json's 10000 the record still reads, and it is
	// the writer's bytes. One container past the interpreter's edge, _read_record raises the
	// RecursionError it does not catch and the reading stops there as a reader fault.
	t.Run("a record nested as deep as Python reads one", func(t *testing.T) {
		deep := func(depth int) any {
			var v any = []any{}
			for range depth - 1 {
				v = []any{v}
			}
			return v
		}
		for _, c := range []struct {
			field string
			depth int
		}{
			{"assignmentId", 10000},
			{"observation", 20000},
			{"guardRecordedAs", pyload.Nesting - 1},
		} {
			h, records := oneEvent(t)
			change(t, records[hook.Accepted], set(c.field, deep(c.depth)))
			code, answer := verify(t, append(roots(h.journal), "--codex-home", h.codex)...)
			expectVerdict(t, code, answer, 0, "TRUE")
			if len(listed(answer, "rowsUnreadable")) != 0 || len(listed(answer, "acceptedRowsMissing")) != 0 {
				t.Fatalf("%s %d deep: %v", c.field, c.depth, answer)
			}
		}
		h, records := oneEvent(t)
		change(t, records[hook.Accepted], set("assignmentId", deep(pyload.Nesting)))
		code, answer := verify(t, append(roots(h.journal), "--codex-home", h.codex)...)
		expectVerdict(t, code, answer, 3, "UNREADABLE")
		if answer["readerFault"] != "RecursionError: maximum recursion depth exceeded while decoding a JSON array from a unicode string" {
			t.Fatal(answer["readerFault"])
		}
	})
	t.Run("a linked day or ledger directory", func(t *testing.T) {
		for _, kind := range []string{hook.Accepted, "claim"} {
			h, records := oneEvent(t)
			real := filepath.Dir(records[kind])
			moved := h.root + "/moved"
			if err := os.Rename(real, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(moved, real); err != nil {
				t.Fatal(err)
			}
			unreadable(t, h)
		}
	})
	for _, c := range []struct {
		name, kind, field string
		form              func(h *host, value string) string
	}{
		{"a relative host ledger", "claim", "claimedBy.hostLedger", func(_ *host, v string) string { return strings.TrimPrefix(v, "/") }},
		{"a dotted host ledger", "claim", "claimedBy.hostLedger", func(_ *host, v string) string { return filepath.Dir(v) + "/./" + filepath.Base(v) }},
		{"a copied host ledger", "claim", "claimedBy.hostLedger", func(h *host, v string) string {
			copied := h.root + "/copy/" + strings.Join(hook.HostLedgerParts, "/")
			if err := os.MkdirAll(copied, 0o755); err != nil {
				h.t.Fatal(err)
			}
			return copied
		}},
		{"a relative journal root", "host", "claimedBy.journalRoot", func(_ *host, v string) string { return strings.TrimPrefix(v, "/") }},
		{"a dotted settings path", hook.Duplicate, "configuration", func(_ *host, v string) string { return filepath.Dir(v) + "/./" + filepath.Base(v) }},
		{"a transcript path with a NUL", hook.Accepted, "eventIdentity.transcriptPath", func(_ *host, v string) string { return v + "\x00bad" }},
		{"a transcript path with a name longer than the system takes", hook.Accepted, "eventIdentity.transcriptPath", func(_ *host, v string) string { return v + "/" + strings.Repeat("x", 300) }},
		{"a transcript path the system cannot encode", hook.Accepted, "eventIdentity.transcriptPath", func(_ *host, v string) string { return v + "\xed\xa0\x80" }},
		{"a settings path with a NUL", hook.Duplicate, "configuration", func(_ *host, v string) string { return v + "\x00bad" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, records := oneEvent(t)
			value := valueAt(t, records[c.kind], c.field).(string)
			change(t, records[c.kind], set(c.field, c.form(h, value)))
			unreadable(t, h)
		})
	}
	for name, place := range map[string]func(row string) []string{
		"hidden.json":        func(row string) []string { return []string{filepath.Dir(row) + "/hidden.json"} },
		"a .bak row":         func(row string) []string { return []string{row + ".bak"} },
		"a nested directory": func(row string) []string { return []string{filepath.Dir(row) + "/sub/" + filepath.Base(row)} },
		"a directory named for row": func(row string) []string {
			return []string{filepath.Dir(row) + "/" + strings.Repeat("0", 32) + ".json/x"}
		},
		"a copied day":             func(row string) []string { return []string{filepath.Dir(row) + ".bak/" + filepath.Base(row)} },
		"a stray file at the root": func(row string) []string { return []string{filepath.Dir(filepath.Dir(row)) + "/stray.json"} },
	} {
		t.Run("an entry the adapter never writes: "+name, func(t *testing.T) {
			h, records := oneEvent(t)
			raw, _ := os.ReadFile(records[hook.Accepted])
			for _, target := range place(records[hook.Accepted]) {
				if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if len(listed(unreadable(t, h), "foreignJournalEntries")) == 0 {
				t.Fatal("the entry was skipped")
			}
		})
	}
}

// SEV-13: a transcript path the system refuses is recorded, and counted, as unreachable.
func TestSEV13_ARefusedTranscriptPathIsCountedAsUnreachable(t *testing.T) {
	h := newHost(t, hook.Release)
	var payload map[string]any
	if err := jsonUnmarshal(h.at(loadFixture(t), 0), &payload); err != nil {
		t.Fatal(err)
	}
	payload["transcript_path"] = h.transcript + "\x00bad"
	h.run(h.settings, jsonMarshal(payload))
	code, answer := verify(t, roots(h.journal)...)
	expectVerdict(t, code, answer, 3, "UNREADABLE")
	if got := answer["unjudgedInvocations"].(map[string]any); len(got) != 1 || got["unestablished:transcript_unreachable"] != 1.0 {
		t.Fatal(got)
	}
	change(t, h.rowPaths(h.journal)[0], set("eventIdentity.reason", "transcript_absent"))
	code, answer = verify(t, roots(h.journal)...)
	expectVerdict(t, code, answer, 3, "UNREADABLE")
	if got := answer["unjudgedInvocations"].(map[string]any); len(got) != 0 {
		t.Fatalf("a reason reached only through a path the system took was counted: %v", got)
	}
}

// SEV-14: a window bound is a time in the records' own format, or a usage error.
func TestSEV14_AWindowBoundIsInTheRecordsFormat(t *testing.T) {
	h, _ := oneEvent(t)
	for _, bound := range []string{"2026-09-23T11:58:17.500Z", "2026-09-23 11:58:17", "2026-09-23T11:58:17+00:00", "2026-99-99T99:99:99Z"} {
		var out, errs bytes.Buffer
		if code := Run(append(roots(h.journal), "--until", bound), &out, &errs); code != 2 || out.Len() != 0 || !strings.Contains(errs.String(), "argument --until: invalid window_bound value") {
			t.Fatalf("%q: exit %d, stdout %q, stderr %q", bound, code, out.String(), errs.String())
		}
		if WindowBound(bound) == nil {
			t.Fatalf("%q was taken as a bound", bound)
		}
	}
	if WindowBound("2026-09-23T11:58:17Z") != nil {
		t.Fatal("a bound in the records' format was refused")
	}
}
