package configguard

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
	"github.com/thisisjun786/codex-relay-workflow/internal/tomledit"
)

// CXC v0.2.40 config-guard/src/multi-agent-v2.ts:15-40, :64-119.
// This library resolves no global home and executes only the injected runner.
type MultiAgentV2Deps struct {
	Run        CodexRunner
	CodexHome  string
	ConfigPath *string // nil selects the default; an explicit empty path stays empty
}

type MultiAgentVersion string

const (
	MultiAgentV1              MultiAgentVersion = "v1"
	MultiAgentV2              MultiAgentVersion = "v2"
	MultiAgentV2AppliesTo                       = "flag-fallback models only"
	MultiAgentV2EffectiveFrom                   = "new sessions"
)

type MultiAgentV2Catalog struct {
	V2 [2]string `json:"v2"`
	V1 [1]string `json:"v1"`
}

// MultiAgentV2CatalogPinned retains the oracle's point-in-time pins, not a live catalog.
func MultiAgentV2CatalogPinned() MultiAgentV2Catalog {
	return MultiAgentV2Catalog{V2: [2]string{"gpt-5.6-sol", "gpt-5.6-terra"}, V1: [1]string{"gpt-5.6-luna"}}
}

type MultiAgentV2Context struct {
	AppliesTo     string              `json:"appliesTo"`
	CatalogPinned MultiAgentV2Catalog `json:"catalogPinned"`
	EffectiveFrom string              `json:"effectiveFrom"`
}

func MultiAgentV2StatusContext() MultiAgentV2Context {
	return MultiAgentV2Context{MultiAgentV2AppliesTo, MultiAgentV2CatalogPinned(), MultiAgentV2EffectiveFrom}
}

type MultiAgentV2State struct {
	Version   MultiAgentVersion `json:"version"`
	V2Enabled bool              `json:"v2Enabled"`
	MultiAgentV2Context
}

type MultiAgentV2Change struct {
	Version   MultiAgentVersion `json:"version"`
	V2Enabled bool              `json:"v2Enabled"`
	Changed   bool              `json:"changed"`
	MultiAgentV2Context
}

func multiAgentV2ConfigPath(deps MultiAgentV2Deps) string {
	if deps.ConfigPath != nil {
		return *deps.ConfigPath
	}
	return filepath.Join(deps.CodexHome, "config.toml")
}

// multiAgentV2EnabledIn reads the flag from the decoded document, the same semantic reader the writers use (CRW-1141):
// [features] multi_agent_v2 = <bool>, or the table [features.multi_agent_v2] (or an inline table) with enabled = <bool>. The
// oracle's line grammar read a header or an enabled line inside a string as configuration and took not_enabled = true for
// enabled; a document that does not decode reads as v1, as a missing or unreadable one does.
func multiAgentV2EnabledIn(content string) bool {
	doc, err := tomledit.Decode(content)
	if err != nil {
		return false
	}
	features, _ := doc["features"].(map[string]any)
	switch v := features["multi_agent_v2"].(type) {
	case bool:
		return v
	case map[string]any:
		enabled, _ := v["enabled"].(bool)
		return enabled
	}
	return false
}

// IsMultiAgentV2Enabled retains readConfigText's missing/unreadable -> v1 behavior.
func IsMultiAgentV2Enabled(configPath string) bool {
	content, exists, err := activationReadFile(configPath)
	return err == nil && exists && multiAgentV2EnabledIn(string(content))
}

func ReadMultiAgentV2State(deps MultiAgentV2Deps) MultiAgentV2State {
	enabled := IsMultiAgentV2Enabled(multiAgentV2ConfigPath(deps))
	version := MultiAgentV1
	if enabled {
		version = MultiAgentV2
	}
	return MultiAgentV2State{version, enabled, MultiAgentV2StatusContext()}
}

// Shield complete multiline values from the oracle helper's trim/filter and
// blank-line compression. Restoring raw blocks also preserves mixed internal EOLs.
func multiAgentV2Preserve(pre, post string, enabled bool) (string, bool) {
	prefix := "CRW_MULTI_AGENT_STRING_"
	for strings.Contains(pre, prefix) || strings.Contains(post, prefix) {
		prefix += "_"
	}
	pairs := []string{}
	shield := func(content string) string {
		lines := text.SplitLines(content)
		mask := tomlInString(append(lines, "")) // sentinel detects an opener at EOF
		raw := text.SplitLinesByteExact(content)
		out := make([]string, 0, len(raw))
		for i := 0; i < len(raw); i++ {
			if !mask[i] && mask[i+1] {
				end := i
				for end+1 < len(raw) && mask[end+1] {
					end++
				}
				block := strings.Join(raw[i:end+1], "\n")
				suffix := ""
				if end+1 < len(raw) && strings.HasSuffix(block, "\r") {
					block = strings.TrimSuffix(block, "\r")
					suffix = "\r"
				}
				token := fmt.Sprintf("%s%d_", prefix, len(pairs)/2)
				pairs = append(pairs, token, block)
				out = append(out, token+suffix)
				i = end
			} else {
				out = append(out, raw[i])
			}
		}
		return strings.Join(out, "\n")
	}
	a, b := shield(pre), shield(post)
	if strings.Contains(pre, "\r\n") && !strings.Contains(a, "\r\n") {
		a += "\r\n" // retain the helper's original EOL choice after shielding
	}
	repaired, changed := PreserveMultiAgentV2Table(a, b, enabled)
	if !changed {
		return "", false
	}
	return strings.NewReplacer(pairs...).Replace(repaired), true
}

// SetMultiAgentV2State refuses unreadable pre/post images and publishes repairs
// atomically. The injected runner remains responsible for its own settings writes.
func SetMultiAgentV2State(deps MultiAgentV2Deps, version MultiAgentVersion) (*MultiAgentV2Change, error) {
	path := multiAgentV2ConfigPath(deps)
	// The pre-image read, the injected runner that rewrites config.toml, the repair and its publish
	// are one critical section under the sidecar lock every CRW writer of config.toml takes
	// (CRW-866), the shape of activate.go's activationSetKeyLocked: a retrust or an activation that
	// published in that window would otherwise be overwritten by the repair, which is computed from
	// the pre-image this read took. An explicitly empty config path names no file, so it is not
	// given a sidecar and keeps its missing-path no-op behaviour.
	var lock *crwdir.ConfigLock
	var dirInfo os.FileInfo
	if path != "" {
		var err error
		lock, err = crwdir.LockConfig(path, activationLockWait)
		if err != nil {
			return nil, err
		}
		defer lock.Release()
		dirInfo, _ = os.Stat(filepath.Dir(path))
		// The lock is keyed by lock.Target, the caller's path with a symlink followed, so two writers
		// reaching one file through different spellings share one lock. The content path stays the
		// caller's (CRW-891), unlike the other writers of config.toml: the runner below may atomically
		// replace the caller's pathname, and the post-image read and the repair publish must then
		// follow the path that names the live config rather than the target the link pointed at when
		// the lock was taken. When that replacement happened the lock guarded the old target while the
		// repair went to the caller's path; the result reports the state read through that path.
	}
	pre, _, err := activationReadFile(path)
	if err != nil {
		return nil, err
	}
	// A config.toml that does not decode is refused before the runner rewrites it (CRW-1141).
	if err := validateConfig(path, string(pre)); err != nil {
		return nil, err
	}
	want := version == MultiAgentV2
	if multiAgentV2EnabledIn(string(pre)) == want {
		return &MultiAgentV2Change{version, want, false, MultiAgentV2StatusContext()}, nil
	}
	op := "disable"
	if want {
		op = "enable"
	}
	res := deps.Run([]string{"features", op, "multi_agent_v2"})
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("codex features %s multi_agent_v2 failed (exit %d): %s", op, res.ExitCode, text.Trim(res.Stderr))
	}
	// The repair is published only under a lock that guards the file the caller's path names after the runner (CRW-1144).
	if lock != nil {
		extra, err := configIdentityAfterRunner(lock, path, dirInfo)
		if err != nil {
			return nil, fmt.Errorf("%w; the runner's change is in place, the multi_agent_v2 tuning repair was not written", err)
		}
		defer extra.Release()
	}
	post, exists, err := activationReadFile(path)
	if err != nil {
		return nil, err
	}
	if exists {
		if repaired, changed := multiAgentV2Preserve(string(pre), string(post), want); changed {
			// The repair is a candidate like any other edit: one that does not decode is not published (CRW-1141).
			if err := validateConfig(path, repaired); err != nil {
				return nil, fmt.Errorf("the multi_agent_v2 tuning repair would leave config.toml invalid, so it was not written: %w", err)
			}
			if err := activationPublish(path, []byte(repaired)); err != nil {
				return nil, err
			}
		}
	}
	// An exit 0 is not proof (CRW-1143): the state is read back from the same config, and a runner that changed nothing,
	// removed the file or wrote the other value is not a change.
	state := ReadMultiAgentV2State(deps)
	if state.V2Enabled != want {
		return nil, fmt.Errorf("codex features %s multi_agent_v2 exited 0, but config.toml still reads %s", op, state.Version)
	}
	return &MultiAgentV2Change{state.Version, state.V2Enabled, multiAgentV2EnabledIn(string(pre)) != state.V2Enabled, state.MultiAgentV2Context}, nil
}
