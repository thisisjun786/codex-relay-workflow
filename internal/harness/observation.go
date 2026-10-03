package harness

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// Entrypoint is the file a record names as its entry and digests. The oracle named the component's
// own script; the Go runtime has none, every leg enters through the crw binary the plugin's hook
// declarations name, so the record names the manifest that declares them.
const Entrypoint = ".codex-plugin/plugin.json"

// jsString is JSON.stringify of a string: quote, backslash and control characters are escaped,
// every other character, U+2028 and U+2029 included, stands as it is.
func jsString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// metadata is hook-observation.mjs' test of a value that may name a session or an actor: a string of
// 1 to 256 UTF-16 units without a control character or space.
func metadata(s string) bool {
	units := 0
	for _, r := range s {
		if r <= 0x20 || r == 0x7f {
			return false
		}
		if units++; r > 0xFFFF {
			units++
		}
	}
	return units > 0 && units <= 256
}

func slug(s string) bool {
	for i, r := range s {
		if !(r >= 'a' && r <= 'z' || i > 0 && (r >= '0' && r <= '9' || r == '-')) {
			return false
		}
	}
	return s != "" && len(s) <= 96
}

func truthy(v any) bool {
	switch v := v.(type) {
	case nil:
		return false
	case bool:
		return v
	case float64:
		return v != 0
	case string:
		return v != ""
	}
	return true
}

// RecordInvocation leaves the metadata-only record that this session or subagent ran this hook
// (scripts/hook-observation.mjs recordHookInvocation): one file per session, actor, component and
// event under <CODEX_HOME>/crw/hook-observations, the latest replacing the one before. It is a
// diagnostic, not an attestation; it retains no payload, never writes to either stream or disturbs
// the hook, and reports only whether a record was written. The plugin is read before any directory
// is made, so a hook outside a plugin leaves nothing behind.
func RecordInvocation(raw, component, event string, env host.LookupEnv) (recorded bool) {
	defer func() {
		if recover() != nil {
			recorded = false
		}
	}()
	var v any
	if len(raw) > MaxStdinBytes || json.Unmarshal([]byte(raw), &v) != nil || !slug(component) || !slug(event) {
		return false
	}
	o, _ := v.(map[string]any)
	session, _ := o["session_id"].(string)
	if !metadata(session) {
		return false
	}
	agent := "null" // the actor's key is its JSON, so no actor and an actor named null differ
	if id := o["agent_id"]; id != nil {
		s, ok := id.(string)
		if !ok || !metadata(s) {
			return false
		}
		agent = jsString(s)
	} else if truthy(o["agent_type"]) {
		return false // a child stamp without its identity never becomes a root record
	}
	root, _ := env("PLUGIN_ROOT")
	if root == "" {
		return false
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	if root, err = filepath.EvalSymlinks(root); err != nil {
		return false
	}
	manifest := filepath.Join(root, Entrypoint)
	if st, err := os.Lstat(manifest); err != nil || !st.Mode().IsRegular() || st.Size() > MaxStdinBytes {
		return false
	}
	data, err := os.ReadFile(manifest)
	if err != nil {
		return false
	}
	var m map[string]any
	if json.Unmarshal(data, &m) != nil {
		return false
	}
	version, _ := m["version"].(string)
	if !metadata(version) {
		return false
	}
	digest := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
	home, set := env("CODEX_HOME")
	if !set {
		h, err := host.Home(env)
		if err != nil {
			return false
		}
		home = filepath.Join(h, ".codex")
	}
	dir := filepath.Join(home, "crw", "hook-observations", digest(session), digest(agent))
	if os.MkdirAll(dir, 0o700) != nil {
		return false
	}
	d := digest(string(data))
	record := `{"schemaVersion":1,"sessionId":` + jsString(session) + `,"agentId":` + agent + `,"component":` + jsString(component) + `,"event":` + jsString(event) +
		`,"observedAt":"` + time.Now().UTC().Format("2006-01-02T15:04:05.000Z") + `","pluginRoot":` + jsString(root) + `,"pluginVersion":` + jsString(version) +
		`,"manifestDigest":"` + d + `","entrypoint":` + jsString(Entrypoint) + `,"entrypointDigest":"` + d + `","outcome":"invoked"}` + "\n"
	suffix := make([]byte, 16)
	if _, err := rand.Read(suffix); err != nil {
		return false
	}
	tmp := filepath.Join(dir, "."+hex.EncodeToString(suffix)+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false
	}
	_, werr := f.WriteString(record)
	slot := digest("[" + jsString(component) + "," + jsString(event) + "," + jsString(Entrypoint) + "]")
	if err := f.Close(); werr != nil || err != nil || os.Rename(tmp, filepath.Join(dir, slot+".json")) != nil {
		_ = os.Remove(tmp)
		return false
	}
	return true
}
