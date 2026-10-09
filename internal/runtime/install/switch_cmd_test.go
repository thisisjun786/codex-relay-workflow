package install_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/hookswitch"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
)

const switchConfig = "[plugins.\"crw@crw\"]\nenabled=true\n\n[plugins.\"codexclaw@codexclaw\"]\nenabled=true\n"

type switchHome struct {
	t    *testing.T
	home string
	env  scope.Env
}

func newSwitchHome(t *testing.T, config string) *switchHome {
	t.Helper()
	home := filepath.Join(t.TempDir(), "codex")
	if err := os.MkdirAll(filepath.Join(home, "agents"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"executor", "architect"} {
		body := "# codexclaw subagent role: " + name + "\n"
		file := fmt.Sprintf("# codexclaw-managed: %x\n%s", sha256.Sum256([]byte(body)), body)
		if err := os.WriteFile(filepath.Join(home, "agents", name+".toml"), []byte(file), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env := scope.Env{}.With("HOME", t.TempDir()).With("CODEX_HOME", home).With("XDG_STATE_HOME", t.TempDir()).With("XDG_CONFIG_HOME", t.TempDir())
	return &switchHome{t: t, home: home, env: env}
}

func (h *switchHome) run(args ...string) (int, string, string) {
	h.t.Helper()
	var stdout, stderr bytes.Buffer
	code := install.Main(context.Background(), append([]string{"switch"}, args...), h.env, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func (h *switchHome) status() install.SwitchStatus {
	h.t.Helper()
	code, out, errOut := h.run("status", "--json")
	if code != 0 {
		h.t.Fatalf("status: exit %d stderr=%q", code, errOut)
	}
	var s install.SwitchStatus
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		h.t.Fatalf("status JSON: %v\n%s", err, out)
	}
	return s
}

func (h *switchHome) config() string {
	b, err := os.ReadFile(filepath.Join(h.home, "config.toml"))
	if err != nil {
		h.t.Fatal(err)
	}
	return string(b)
}

func TestInstallSwitchIsARoutedCommandWithItsOwnHelp(t *testing.T) {
	found := false
	for _, c := range install.Commands {
		found = found || c == "switch"
	}
	if !found {
		t.Fatalf("Commands = %v, want switch", install.Commands)
	}
	var stdout, stderr strings.Builder
	for _, args := range [][]string{{"switch", "--help"}, {"switch", "crw", "-h"}, {"switch", "help"}} {
		stdout.Reset()
		if code := install.Main(context.Background(), args, nil, &stdout, &stderr); code != 0 || !strings.HasPrefix(stdout.String(), "Usage:\n  crw install switch crw") || stderr.Len() != 0 {
			t.Fatalf("%v: exit %d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
		}
	}
	stdout.Reset()
	if code := install.Main(context.Background(), []string{"help"}, nil, &stdout, &strings.Builder{}); code != 0 || stdout.String() != "usage: crw install {install,update,rollback,remove,status,register-mcp,hook,register-service} ...\n" {
		t.Fatalf("the frozen install usage changed: %q", stdout.String())
	}
	h := newSwitchHome(t, switchConfig)
	for _, args := range [][]string{{}, {"both"}, {"crw", "cxc"}, {"--nope"}} {
		if code, _, _ := h.run(args...); code != 2 {
			t.Fatalf("switch %v: exit %d, want 2", args, code)
		}
	}
}

func TestInstallSwitchRoundTripThroughTheCommand(t *testing.T) {
	h := newSwitchHome(t, switchConfig)
	if s := h.status(); s.State != "cxc" || s.Switch.Active != "" || !s.Plugins["codexclaw"].Enabled || !s.Plugins["crw"].Enabled {
		t.Fatalf("initial status = %+v", s)
	}
	code, out, errOut := h.run("crw", "--json")
	if code != 0 {
		t.Fatalf("switch crw: exit %d stderr=%q", code, errOut)
	}
	var res struct {
		OK     bool   `json:"ok"`
		Action string `json:"action"`
		Active string `json:"active"`
		Keys   []struct{ Before, After string }
		Roles  []struct{ Role, Action string }
		Status install.SwitchStatus
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !res.OK || res.Action != "crw" || res.Active != "crw" || len(res.Keys) != 1 || res.Keys[0].Before != "true" || res.Keys[0].After != "false" || len(res.Roles) != 2 || res.Status.State != "crw" {
		t.Fatalf("switch crw result = %+v", res)
	}
	for _, r := range res.Roles {
		if r.Action != "replaced-cxc" {
			t.Fatalf("role %+v", r)
		}
	}
	s := h.status()
	if s.State != "crw" || s.Switch.Active != "crw" || s.Plugins["codexclaw"].Enabled || !s.Plugins["codexclaw"].Present || !s.Plugins["crw"].Enabled || !s.Record.Present || s.Record.Pending {
		t.Fatalf("status after crw = %+v", s)
	}
	for _, r := range s.Roles {
		if r.Owner != "crw" {
			t.Fatalf("role %+v", r)
		}
	}
	if !strings.Contains(h.config(), "[plugins.\"crw@crw\"]\nenabled=true\n") {
		t.Fatalf("crw@crw table touched:\n%s", h.config())
	}
	// Plain text names the state too.
	if code, text, _ := h.run("status"); code != 0 || !strings.Contains(text, "state: crw\n") || !strings.Contains(text, "plugin codexclaw@codexclaw: disabled") || !strings.Contains(text, "role executor: crw") {
		t.Fatalf("status text:\n%s", text)
	}
	if code, _, errOut := h.run("cxc"); code != 0 {
		t.Fatalf("switch cxc: exit %d stderr=%q", code, errOut)
	}
	if got := h.config(); got != switchConfig {
		t.Fatalf("config differs after the round trip:\n%q", got)
	}
	if s := h.status(); s.State != "cxc" || s.Switch.Active != "cxc" {
		t.Fatalf("status after cxc = %+v", s)
	}
}

// The file crw install switch writes is the file the hooks read: hookswitch reports the side the
// command picked, with nothing it could not read.
func TestInstallSwitchWritesWhatTheHooksRead(t *testing.T) {
	h := newSwitchHome(t, switchConfig)
	env := func(k string) (string, bool) {
		if k == "CODEX_HOME" {
			return h.home, true
		}
		return "", false
	}
	if r := hookswitch.Read(env); r.On || r.Problem != "" || r.CodexHome != h.home {
		t.Fatalf("before any switch the hooks read %+v, want off", r)
	}
	for _, c := range []struct {
		side string
		on   bool
	}{{hookswitch.CRW, true}, {hookswitch.CXC, false}, {hookswitch.CRW, true}} {
		if code, _, errOut := h.run(c.side); code != 0 {
			t.Fatalf("switch %s: exit %d stderr=%q", c.side, code, errOut)
		}
		st, err := hookswitch.Load(h.home)
		if err != nil || st == nil || st.Active != c.side || st.By != "crw install switch" || st.ChangedAt == "" {
			t.Fatalf("switch %s: hookswitch.Load = %+v, %v", c.side, st, err)
		}
		if r := hookswitch.Read(env); r.On != c.on || r.Problem != "" {
			t.Fatalf("switch %s: the hooks read %+v, want on=%v", c.side, r, c.on)
		}
		raw, err := os.ReadFile(hookswitch.Path(h.home))
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]string
		if err := json.Unmarshal(raw, &doc); err != nil || doc["active"] != c.side || len(doc) != 3 {
			t.Fatalf("switch %s: switch.json = %q, %v", c.side, raw, err)
		}
	}
}

func TestInstallSwitchStatusNamesConflictAndOff(t *testing.T) {
	h := newSwitchHome(t, switchConfig)
	if code, _, e := h.run("crw"); code != 0 {
		t.Fatal(e)
	}
	// What `codex plugin update` might do: turn the key back on.
	if err := os.WriteFile(filepath.Join(h.home, "config.toml"), []byte(strings.Replace(h.config(), "enabled = false", "enabled = true", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if s := h.status(); s.State != "conflict" || len(s.Notes) == 0 {
		t.Fatalf("status = %+v", s)
	}
	if code, _, e := h.run("crw"); code != 0 {
		t.Fatal(e)
	}
	if s := h.status(); s.State != "crw" {
		t.Fatalf("status after the switch put it right = %+v", s)
	}
	// Neither side on.
	off := newSwitchHome(t, "[plugins.\"codexclaw@codexclaw\"]\nenabled = false\n")
	if s := off.status(); s.State != "off" {
		t.Fatalf("status = %+v", s)
	}
	// A damaged switch.json is reported, not guessed.
	bad := newSwitchHome(t, switchConfig)
	if err := os.MkdirAll(filepath.Dir(hookswitch.Path(bad.home)), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hookswitch.Path(bad.home), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s := bad.status(); s.Switch.Error == "" || s.State != "cxc" {
		t.Fatalf("status = %+v", s)
	}
}

func TestInstallSwitchStatusSummarisesHookTrust(t *testing.T) {
	h := newSwitchHome(t, switchConfig)
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(".codex-plugin/plugin.json", `{"name":"crw","hooks":["./wiring/hooks/a.json"]}`)
	write("wiring/hooks/a.json", `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"crw hook --plugin-launch; exit 0","timeout":10}]}]}}`)
	entries, err := doctor.ListHookTrustEntries(root, "crw@crw")
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %v, %v", entries, err)
	}
	trust := func() install.SwitchStatus {
		code, out, e := h.run("status", "--json", "--plugin-root", root)
		if code != 0 {
			t.Fatal(e)
		}
		var s install.SwitchStatus
		if err := json.Unmarshal([]byte(out), &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if s := trust(); !s.HookTrust.Available || s.HookTrust.Untrusted != 1 || s.HookTrust.Trusted != 0 || s.HookTrust.Key != "crw@crw" {
		t.Fatalf("no record: %+v", s.HookTrust)
	}
	section := func(hash string) string {
		return fmt.Sprintf("\n[hooks.state.%q]\ntrusted_hash = %q\n", entries[0].Key, hash)
	}
	if err := os.WriteFile(filepath.Join(h.home, "config.toml"), []byte(switchConfig+section(entries[0].Hash)), 0o600); err != nil {
		t.Fatal(err)
	}
	if s := trust(); s.HookTrust.Trusted != 1 || s.HookTrust.Drifted != 0 || s.HookTrust.Untrusted != 0 {
		t.Fatalf("trusted: %+v", s.HookTrust)
	}
	if err := os.WriteFile(filepath.Join(h.home, "config.toml"), []byte(switchConfig+section("sha256:other")), 0o600); err != nil {
		t.Fatal(err)
	}
	if s := trust(); s.HookTrust.Drifted != 1 || s.HookTrust.Trusted != 0 {
		t.Fatalf("drifted: %+v", s.HookTrust)
	}
	// Without a package to read, trust is reported unavailable, not guessed.
	if s := h.status(); s.HookTrust.Available || s.HookTrust.Reason == "" {
		t.Fatalf("no package: %+v", s.HookTrust)
	}
}

func TestInstallSwitchRefusalWritesNothing(t *testing.T) {
	h := newSwitchHome(t, "[plugins.\"codexclaw@codexclaw\"]\nenabled = [true]\n")
	var stdout, stderr strings.Builder
	code := install.Main(context.Background(), []string{"switch", "crw"}, h.env, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "will not rewrite") {
		t.Fatalf("exit %d stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(hookswitch.Path(h.home)); !os.IsNotExist(err) {
		t.Fatalf("switch.json written by a refused switch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.home, ".crw-install.json")); !os.IsNotExist(err) {
		t.Fatalf("manifest written by a refused switch: %v", err)
	}
}

// The CRW side counts as on only while the CRW plugin is enabled (verification finding 5).
func TestInstallSwitchStatusReadsTheCRWPluginsOwnKey(t *testing.T) {
	selectCRW := func(t *testing.T, h *switchHome) {
		t.Helper()
		if err := hookswitch.Write(h.home, hookswitch.State{Active: hookswitch.CRW, ChangedAt: "2026-10-10T01:00:00.000Z", By: "crw install switch"}); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("crw off, cxc on", func(t *testing.T) {
		h := newSwitchHome(t, "[plugins.\"crw@crw\"]\nenabled=false\n\n[plugins.\"codexclaw@codexclaw\"]\nenabled=true\n")
		selectCRW(t, h)
		s := h.status()
		if s.State != "cxc" || len(s.Notes) == 0 {
			t.Fatalf("status = %+v", s)
		}
	})
	t.Run("both off", func(t *testing.T) {
		h := newSwitchHome(t, "[plugins.\"crw@crw\"]\nenabled=false\n\n[plugins.\"codexclaw@codexclaw\"]\nenabled=false\n")
		selectCRW(t, h)
		if s := h.status(); s.State != "off" {
			t.Fatalf("status = %+v", s)
		}
	})
	t.Run("crw on, cxc on is still a conflict", func(t *testing.T) {
		h := newSwitchHome(t, switchConfig)
		selectCRW(t, h)
		if s := h.status(); s.State != "conflict" {
			t.Fatalf("status = %+v", s)
		}
	})
}
