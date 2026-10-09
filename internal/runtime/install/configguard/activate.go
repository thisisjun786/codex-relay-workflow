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
	return activationPublished(activationCrwdirPublish(path, b))
}

// activationCrwdirPublish is the write of every file this package publishes; a test replaces it to stage a
// publication whose directory sync failed.
var activationCrwdirPublish = crwdir.Publish

// activationPublished counts a publication whose file is in place as done. The activation, the key edits and the
// deactivation read what they published and write the install manifest after it: failing after the rename would
// leave config.toml changed with no manifest to undo it from, which is worse than a file whose directory sync is
// unknown, and the files are not read back on the strength of a power failure (CRW-802).
func activationPublished(err error) error {
	if crwdir.Published(err) {
		return nil
	}
	return err
}

// configLockPathsPublishChecked is activationPublish with check run at the last step, after the new content
// is written and synced and immediately before the rename. A refusal publishes nothing (CRW-993 c1).
func configLockPathsPublishChecked(path string, b []byte, check func() error) error {
	if _, _, e := activationReadFile(path); e != nil {
		return e
	}
	return activationPublished(crwdir.PublishChecked(path, b, check))
}

// activationSetKeyLocked is the whole read-modify-write of one auto-enabled key under the sidecar
// lock every CRW writer of config.toml takes (CRW-844): the read, the decision and the publish are
// serialized against retrust and any other CRW writer, so two writers never interleave on one
// config.toml, and a retrust that publishes between this read and this write cannot be overwritten
// with content built from the pre-retrust bytes. The lock is taken once by Activate and held across
// the whole flow (CRW-877), so this helper takes no lock of its own: taking one here would be the
// activation deadlocking on the lock it already holds. The other files this package publishes (the
// install manifest, the self-heal marker) are not shared with another writer and keep
// activationPublish.
func activationSetKeyLocked(path, table, key string) (TomlEditResult, error) {
	content, _, e := activationReadFile(path)
	if e != nil {
		return TomlEditResult{}, e
	}
	res := SetTableKey(string(content), table, key, true)
	if !res.Changed {
		return res, nil
	}
	if e := activationPublish(path, []byte(res.Content)); e != nil {
		return TomlEditResult{}, e
	}
	return res, nil
}

// activationLockWait is how long the activation publish waits for another CRW writer's sidecar lock.
const activationLockWait = 2 * time.Second

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
	// The staging file is renamed again below, so its own directory sync says nothing about the backup; the sync
	// after the final rename does.
	if err = crwdir.Publish(tmp, b); err != nil && !crwdir.Published(err) {
		return err
	}
	if err = os.Chmod(tmp, mode.Perm()); err != nil {
		return err
	}
	if err = crwdir.Rename(tmp, target); err != nil {
		return err
	}
	// The backup is what a later restore reads, and config.toml is rewritten next: a backup whose entry may not
	// survive a power failure stops the activation here, before config.toml changes (CRW-802). A directory that
	// cannot be opened for the sync (mode 0300) is such a backup too, so its permission error is returned as well;
	// this differs from activationPublished, which counts a published config.toml or manifest as done because the
	// files after it depend on it, while nothing has been changed yet when the backup fails.
	return crwdir.SyncDir(filepath.Dir(target))
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
		// The oracle derives this path the same way (activate.ts:201, deps.configPath ??
		// join(codexHome, "config.toml")), and Node's path.join folds a ".." lexically exactly as
		// filepath.Join does. Resolving it through the kernel here would name a different file
		// from the oracle for a CODEX_HOME that contains a symlink followed by "..", so the port
		// keeps the oracle's derivation; the shared limitation is recorded in
		// docs/port-cxc/known-defects/CRW-899.md (parity wins).
		path = filepath.Join(deps.CodexHome, "config.toml")
	}
	now := deps.Now
	if now == nil {
		now = func() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }
	}
	if e := os.MkdirAll(deps.CodexHome, 0777); e != nil {
		return nil, e
	}
	// One critical section for the whole activation, under the sidecar lock every CRW writer of
	// config.toml takes (CRW-877): the pre-install read, the backup, the injected "codex features
	// enable" calls that rewrite config.toml themselves, the managed-key read-modify-writes and the
	// post-activation hash. A CRW writer that published between any two of those would otherwise have
	// its change discarded by the CLI's own read-modify-write, with neither command reporting it. The
	// wait and the busy text are the ones the other writers use.
	lock, e := crwdir.LockConfig(path, activationLockWait)
	if e != nil {
		// Contention is answered as it is, before the re-read below: LockConfig builds it once with
		// errors.New(crwdir.ConfigLockBusy) and never wraps it (internal/pabcd/crwdir/swap.go), so the
		// exact comparison is the whole test. A busy lock is not a read failure, and mapping it to the
		// read path's message would hide which writer the operator is waiting behind.
		if e.Error() == crwdir.ConfigLockBusy {
			return nil, e
		}
		// A file this activation cannot read is refused with the read path's own message, which the
		// tests and the operator already know ("left unchanged"); LockConfig resolves the target
		// through a symlink, so a config.toml link that leads nowhere fails here rather than in the
		// read. Only a failure that is not contention reaches this re-read.
		if _, _, readErr := activationReadFile(path); readErr != nil {
			return nil, readErr
		}
		return nil, e
	}
	defer lock.Release()
	// The lock is keyed by lock.Target, the caller's path with a symlink followed, so two writers
	// reaching one file through different spellings share one lock. The content path stays the
	// caller's (CRW-899), the rule CRW-891 gave SetMultiAgentV2State: the injected "codex features
	// enable" calls below rewrite config.toml themselves and may atomically replace the caller's
	// pathname, so the managed-key read-modify-writes and the post-activation hash must follow the
	// path that names the live config rather than the target the link pointed at when the lock was
	// taken. When that replacement happened the lock guarded the old target while the keys went to
	// the caller's path, which is the limitation recorded in docs/port-cxc/known-defects/CRW-899.md.
	// The declared-state probe reads the same config.toml through the injected CLI, and its answer
	// decides both which flags are enabled below and every flag's priorEnabled, so it runs inside the
	// critical section too. A probe taken before the wait would let an activation that holds the lock
	// enable a flag and publish its manifest, after which this activation would enable the flag again,
	// record it as previously disabled and claim it as its own, and a later deactivation would turn
	// off a flag the other activation enabled.
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
		// The whole read-modify-write is under the sidecar lock every CRW writer of config.toml
		// takes (CRW-844): reading before the lock and publishing after it would let a retrust that
		// published in that window be overwritten with content built from the pre-retrust bytes.
		res, e := activationSetKeyLocked(path, entry.Table, entry.Key)
		if e != nil {
			return nil, e
		}
		if res.Action == TomlUnsupportedValue {
			continue
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
	out = regexp.MustCompile(`(?:\r?\n){3,}`).ReplaceAllString(out, eol+eol)
	out = regexp.MustCompile(`(?:\r?\n)*$`).ReplaceAllString(out, eol)
	return out + eol + "[features.multi_agent_v2]" + eol + "enabled = " + value + eol + strings.Join(lines, eol) + eol, true
}
func strconvBool(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
