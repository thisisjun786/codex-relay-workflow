package evidence

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// attemptsPath is the counter file of an agent in a turn: the sanitised ids, kept so that a person can read the directory, and
// a digest of the raw pair, because sanitising is not injective ("a/b" and "a-b" are one string). An absent turn has the same
// name shape without the turn part.
func attemptsPath(cwd, sessionID, agentID, turnID string) string {
	name := state.SanitizeKey(sessionID) + "-" + state.SanitizeKey(agentID) + "-"
	if turnID != "" {
		name += state.SanitizeKey(turnID) + "-"
	}
	return filepath.Join(cwd, crwdir.DirName, AttemptsSubdir, name+tupleDigest(agentID, turnID)+".json")
}

// tupleDigest is the first 32 hex digits of the SHA-256 of "<len>:<agent>:<len>:<turn>" taken as UTF-16 code units in
// little-endian order, the lengths counted in code units, as the oracle's Buffer.from(.., "utf16le") hashes it.
func tupleDigest(agentID, turnID string) string {
	units := func(s string) []uint16 { return utf16.Encode([]rune(s)) }
	agent, turn := units(agentID), units(turnID)
	framed := append(units(fmt.Sprintf("%d:", len(agent))), agent...)
	framed = append(framed, units(fmt.Sprintf(":%d:", len(turn)))...)
	framed = append(framed, turn...)
	raw := make([]byte, 0, 2*len(framed))
	for _, u := range framed {
		raw = binary.LittleEndian.AppendUint16(raw, u)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:16])
}

// ReadAttempts is the number of blocks already spent for the (session, agent, turn); an empty turnID is an absent turn. A file
// that is absent, unreadable or not JSON is 0 (so a truncated counter restarts the budget). A JSON object or array whose
// attempts is not a safe integer, or lies outside 0 to MaxAttempts, is MaxAttempts: corrupt verification data ends the budget,
// it never extends it. Any other JSON value (null, a number, a string, true) is 0.
func ReadAttempts(cwd, sessionID, agentID, turnID string) int {
	raw, err := os.ReadFile(attemptsPath(cwd, sessionID, agentID, turnID))
	if err != nil {
		return 0
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		if strings.Contains(err.Error(), "exceeded max depth") { // JSON.parse has no such limit
			return MaxAttempts
		}
		return 0
	}
	if _, err := dec.Token(); err != io.EOF {
		return 0
	}
	var attempts any
	switch o := v.(type) {
	case map[string]any:
		attempts = o["attempts"]
	case []any:
	default:
		return 0
	}
	n, isNumber := attempts.(json.Number)
	f, _ := strconv.ParseFloat(string(n), 64) // beyond float64 is ±Inf, as in JavaScript
	if !isNumber || math.IsInf(f, 0) || f != math.Trunc(f) || math.Abs(f) > 1<<53-1 || f < 0 || f > MaxAttempts {
		return MaxAttempts
	}
	return int(f)
}

// WriteAttempts persists the counter through a temp file and a rename, and reports whether it did: a false means nothing durable
// was written, and the caller ends the budget instead of blocking again on a counter it cannot advance. A failed rename leaves
// its temp file behind.
func WriteAttempts(cwd, sessionID, agentID string, attempts int, turnID string) bool {
	path := attemptsPath(cwd, sessionID, agentID, turnID)
	if _, err := crwdir.EnsureDir(cwd); err != nil {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return false
	}
	tmp := fmt.Sprintf("%s.%d.%s.tmp", path, os.Getpid(), rand.Text())
	if err := os.WriteFile(tmp, []byte(fmt.Sprintf("{\"attempts\":%d}\n", attempts)), 0o666); err != nil {
		return false
	}
	return crwdir.Rename(tmp, path) == nil
}

// ClearAttempts removes the counter file, best effort: a missing file is fine and a directory in its place stays.
func ClearAttempts(cwd, sessionID, agentID, turnID string) {
	path := attemptsPath(cwd, sessionID, agentID, turnID)
	if info, err := os.Lstat(path); err == nil && !info.IsDir() {
		_ = os.Remove(path)
	}
}
