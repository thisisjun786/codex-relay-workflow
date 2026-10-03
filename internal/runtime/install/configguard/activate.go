package configguard

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

type ActivateDeps struct {
	Run                   CodexRunner
	CodexHome, ConfigPath string
	Now                   func() string
}

// activationRead distinguishes an absent file from an unreadable file or dangling link.
func activationReadFile(path string) ([]byte, bool, error) {
	b, e := os.ReadFile(path)
	if e == nil {
		return b, true, nil
	}
	if errors.Is(e, fs.ErrNotExist) {
		if _, lerr := os.Lstat(path); errors.Is(lerr, fs.ErrNotExist) {
			return nil, false, nil
		}
	}
	return nil, false, fmt.Errorf("could not read %s (left unchanged): %w", path, e)
}
func hashOrNull(path string) (*string, error) {
	b, exists, e := activationReadFile(path)
	if e != nil || !exists {
		return nil, e
	}
	sum := sha256.Sum256(b)
	h := hex.EncodeToString(sum[:])
	return &h, nil
}
func readPriorManifest(home string) (*InstallManifest, error) {
	b, _, e := activationReadFile(manifestPath(home))
	if e != nil {
		return nil, e
	}
	return parseInstallManifest(string(b)), nil
}
func activationPublish(path string, b []byte) error {
	if _, _, e := activationReadFile(path); e != nil {
		return e
	}
	return crwdir.Publish(path, b)
}

// activationBackup keeps staging private through Publish, then gives it the source mode.
// Publish owns content writes and fsync; Rename publishes the completed backup, preserving links.
func activationBackup(path string, b []byte, mode fs.FileMode) (err error) {
	target := path
	if info, e := os.Lstat(path); e == nil && info.Mode()&os.ModeSymlink != 0 {
		target, e = filepath.EvalSymlinks(path)
		if e != nil {
			return e
		}
	}
	if _, exists, e := activationReadFile(target); e != nil {
		return e
	} else if exists {
		f, e := os.OpenFile(target, os.O_WRONLY, 0)
		if e != nil {
			return e
		}
		if e = f.Close(); e != nil {
			return e
		}
	}
	f, e := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".*.tmp")
	if e != nil {
		return e
	}
	tmp := f.Name()
	defer func() {
		if e := os.Remove(tmp); e != nil && !errors.Is(e, fs.ErrNotExist) {
			err = errors.Join(err, e)
		}
	}()
	if err = f.Close(); err != nil {
		return err
	}
	if err = crwdir.Publish(tmp, b); err != nil {
		return err
	}
	if err = os.Chmod(tmp, mode.Perm()); err != nil {
		return err
	}
	return crwdir.Rename(tmp, target)
}

func activationFailureMessage(s string) string {
	s = text.Trim(s)
	units := 0
	for i := 0; i < len(s); {
		r, n := pyjson.CodePoint(s, i)
		width := 1
		if r > 0xffff {
			width = 2
		}
		if units+width > 500 {
			if units == 499 {
				high := 0xd800 + (r-0x10000)>>10
				return s[:i] + string([]byte{0xed, byte(0x80 | (high>>6)&0x3f), byte(0x80 | high&0x3f)})
			}
			return s[:i]
		}
		units += width
		i += n
	}
	return s
}

// Activate is the injected activation orchestration. It never resolves a home or starts a binary.
func Activate(deps ActivateDeps) (*InstallManifest, error) {
	path := deps.ConfigPath
	if path == "" {
		path = filepath.Join(deps.CodexHome, "config.toml")
	}
	now := deps.Now
	if now == nil {
		now = func() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }
	}
	if e := os.MkdirAll(deps.CodexHome, 0777); e != nil {
		return nil, e
	}
	state, e := ReadDeclaredState(deps.Run)
	if e != nil {
		return nil, e
	}
	pre, exists, e := activationReadFile(path)
	if e != nil {
		return nil, e
	}
	var backup *string
	if exists {
		info, e := os.Stat(path)
		if e != nil {
			return nil, e
		}
		name := path + ".crw-" + strings.NewReplacer(":", "-", ".", "-").Replace(now()) + ".bak"
		if e = activationBackup(name, pre, info.Mode()); e != nil {
			return nil, e
		}
		backup = &name
	}
	prior, e := readPriorManifest(deps.CodexHome)
	if e != nil {
		return nil, e
	}
	m := &InstallManifest{Version: 2, ConfigPath: path, BackupPath: backup, Flags: map[string]FlagRecord{}, TableKeys: map[string]TableKeyRecord{}}
	for _, k := range DeclaredFeatures() {
		key := string(k)
		m.Flags[key] = FlagRecord{PriorEnabled: state[key]}
		m.flagOrder = append(m.flagOrder, key)
	}
	for _, key := range FeaturesToEnable(state) {
		r := deps.Run([]string{"features", "enable", string(key)})
		f := m.Flags[string(key)]
		if r.ExitCode == 0 {
			f.EnabledByCodexclaw = true
		} else {
			f.EnableFailed = true
			f.Failure = &FailureRecord{float64(r.ExitCode), activationFailureMessage(r.Stderr)}
			if !slices.Contains(SoftFeatures(), key) {
				return nil, fmt.Errorf("codex features enable %s failed (exit %d): %s", key, r.ExitCode, text.Trim(r.Stderr))
			}
		}
		m.Flags[string(key)] = f
	}
	for _, entry := range AutoEnabledManagedKeys() {
		id := ManagedKeyID(entry)
		value, found := ReadTableKey(string(pre), entry.Table, entry.Key)
		var priorValue *string
		if found {
			priorValue = &value
		}
		content, _, e := activationReadFile(path)
		if e != nil {
			return nil, e
		}
		res := SetTableKey(string(content), entry.Table, entry.Key, true)
		if res.Action == TomlUnsupportedValue {
			continue
		}
		if res.Changed {
			if e = activationPublish(path, []byte(res.Content)); e != nil {
				return nil, e
			}
		}
		owned := res.Changed
		if prior != nil {
			if carried, ok := prior.TableKeys[id]; ok {
				priorValue = carried.PriorValue
				owned = carried.SetByCodexclaw || res.Changed
			}
		}
		m.TableKeys[id] = TableKeyRecord{entry.Table, entry.Key, priorValue, "true", owned}
		m.tableOrder = append(m.tableOrder, id)
	}
	m.ActivatedAt = now()
	m.PostActivateHash, e = hashOrNull(path)
	if e != nil {
		return nil, e
	}
	b, e := manifestBytes(m)
	if e != nil {
		return nil, e
	}
	if e = activationPublish(manifestPath(deps.CodexHome), b); e != nil {
		return nil, e
	}
	return m, nil
}

const activationLineEnd = "[\\r\\n\\x{2028}\\x{2029}]"

// activationMatch emulates JS multiline anchors, which also delimit CR, U+2028 and U+2029.
// The captured match excludes the consumed leading/trailing boundary characters.
func activationMatch(s, pattern string) (int, int, bool) {
	re := regexp.MustCompile("(?:^|" + activationLineEnd + ")(" + pattern + ")(?:$|" + activationLineEnd + ")")
	idx := re.FindStringSubmatchIndex(s)
	if idx == nil {
		return 0, 0, false
	}
	return idx[2], idx[3], true
}

func PreserveMultiAgentV2Table(pre, post string, enabled ...bool) (string, bool) {
	want := true
	if len(enabled) > 0 {
		want = enabled[0]
	}
	eol := "\n"
	if strings.Contains(pre, "\r\n") {
		eol = "\r\n"
	}
	header := regexp.QuoteMeta("[features.multi_agent_v2]")
	if _, _, ok := activationMatch(post, header+tomlSpace+"*"); ok {
		return "", false
	}
	horizontal := "[\\t\\v\\f \\x{a0}\\x{1680}\\x{2000}-\\x{200a}\\x{2028}\\x{2029}\\x{202f}\\x{205f}\\x{3000}\\x{feff}]"
	// Header newline is consumed when present; the JS $ alternative can also precede a lone terminator.
	re := regexp.MustCompile("(?:^|" + activationLineEnd + ")(" + header + horizontal + "*)(\\r?\\n|$|" + activationLineEnd + ")")
	idx := re.FindStringSubmatchIndex(pre)
	if idx == nil {
		return "", false
	}
	start := idx[5]
	ending := pre[idx[4]:idx[5]]
	if ending != "\n" && ending != "\r\n" {
		start = idx[3]
	}
	end := start
	for end < len(pre) {
		rest := pre[end:]
		if strings.HasPrefix(strings.TrimLeftFunc(rest, func(r rune) bool { return r != '\r' && r != '\n' && tomlIsSpace(r) }), "[") {
			break
		}
		n := strings.IndexAny(rest, "\r\n")
		if n < 0 {
			end = len(pre)
			break
		}
		if rest[n] == '\r' && (n+1 >= len(rest) || rest[n+1] != '\n') {
			end += n
			break
		}
		end += n + 1
		if rest[n] == '\r' {
			end++
		}
	}
	lines := make([]string, 0)
	for _, line := range text.SplitLines(pre[start:end]) {
		line = text.Trim(line)
		if line != "" && !strings.HasPrefix(line, "#") && !regexp.MustCompile("^enabled"+tomlSpace+"*=").MatchString(line) {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return "", false
	}
	value := strconvBool(want)
	a, b, ok := activationMatch(post, "(?:features\\.)?multi_agent_v2"+tomlSpace+"*="+tomlSpace+"*"+value+tomlSpace+"*")
	if !ok {
		return "", false
	}
	out := post[:a] + post[b:]
	out = regexp.MustCompile("(?:\\r?\\n){3,}").ReplaceAllString(out, eol+eol)
	out = regexp.MustCompile("(?:\\r?\\n)*$").ReplaceAllString(out, eol)
	return out + eol + "[features.multi_agent_v2]" + eol + "enabled = " + value + eol + strings.Join(lines, eol) + eol, true
}
func strconvBool(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
