// Package guidancerecord remembers which SessionStart guidance a session has been given, so a resumed session is
// not given the same text again (CRW-1146). A record is the SHA-256 of the text a hook leg delivered, kept per session
// and leg under <CODEX_HOME>/crw/session-guidance; it holds no text and no payload. Every failure answers "not
// delivered", so a missing, unreadable or changed record makes the leg say the whole thing, as it did before records.
package guidancerecord

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

const dirName = "session-guidance"

func digest(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

// slot is the record file for a session and a leg, or "" when the host has no home, the session is not an identifier
// (1 to 256 UTF-16 units without a control character or space, as the hook observation requires) or the leg is not a slug.
func slot(env host.LookupEnv, session, leg string) string {
	if n := len(utf16.Encode([]rune(session))); n == 0 || n > 256 || leg == "" || len(leg) > 96 {
		return ""
	}
	for _, r := range session {
		if r <= 0x20 || r == 0x7f {
			return ""
		}
	}
	for _, r := range leg {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return ""
		}
	}
	home, set := env("CODEX_HOME")
	if !set {
		h, err := host.Home(env)
		if err != nil {
			return ""
		}
		home = filepath.Join(h, ".codex")
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, "crw", dirName, digest(session), leg)
}

// Delivered reports whether this session was given exactly this text by this leg.
func Delivered(env host.LookupEnv, session, leg, text string) bool {
	path := slot(env, session, leg)
	if path == "" {
		return false
	}
	if st, err := os.Lstat(path); err != nil || !st.Mode().IsRegular() || st.Size() > 128 {
		return false
	}
	data, err := os.ReadFile(path)
	return err == nil && strings.TrimSpace(string(data)) == digest(text)
}

// Record notes that this session was given this text by this leg, replacing the leg's earlier record. It is best effort.
func Record(env host.LookupEnv, session, leg, text string) {
	path := slot(env, session, leg)
	if path == "" || os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return
	}
	tmp := filepath.Join(filepath.Dir(path), "."+hex.EncodeToString(suffix)+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return
	}
	_, werr := f.WriteString(digest(text) + "\n")
	if err := f.Close(); werr != nil || err != nil || os.Rename(tmp, path) != nil {
		_ = os.Remove(tmp)
	}
}
