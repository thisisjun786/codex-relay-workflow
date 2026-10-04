package configguard

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
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

// The read-only oracle grammar (toml-edit.ts:52-69) is deliberately string-unaware.
// TomlTableBody's string-aware writer fix must not change this reader's verdicts.
func multiAgentV2TableBody(content, header string) (string, bool) {
	lines := text.SplitLines(content)
	re := regexp.MustCompile("^" + tomlSpace + "*\\[" + regexp.QuoteMeta(header) + "\\]" + tomlSpace + "*(?:#" + tomlNotEOL + "*)?(?:$|" + activationLineEnd + "$)")
	for i, line := range lines {
		if !re.MatchString(line) {
			continue
		}
		end := i + 1
		for end < len(lines) && !strings.HasPrefix(tomlTrimStart(lines[end]), "[") {
			end++
		}
		return strings.Join(lines[i+1:end], "\n"), true
	}
	return "", false
}

func multiAgentV2Bool(body, key string) (bool, bool) {
	re := regexp.MustCompile("(?:^|" + activationLineEnd + ")" + tomlSpace + "*" + key + tomlSpace + "*=" + tomlSpace + "*(true|false)" + tomlSpace + "*(?:#" + tomlNotEOL + "*)?(?:$|" + activationLineEnd + ")")
	match := re.FindStringSubmatch(body)
	if match == nil {
		return false, false
	}
	return match[1] == "true", true
}

func multiAgentV2EnabledIn(content string) bool {
	if body, ok := multiAgentV2TableBody(content, "features.multi_agent_v2"); ok {
		enabled, _ := multiAgentV2Bool(body, "enabled")
		return enabled
	}
	if body, ok := multiAgentV2TableBody(content, "features"); ok {
		if enabled, found := multiAgentV2Bool(body, "multi_agent_v2"); found {
			return enabled
		}
		inline := regexp.MustCompile("(?:^|" + activationLineEnd + ")" + tomlSpace + "*multi_agent_v2" + tomlSpace + "*=" + tomlSpace + "*\\{([^}]*)\\}").FindStringSubmatch(body)
		if inline != nil {
			match := regexp.MustCompile("enabled" + tomlSpace + "*=" + tomlSpace + "*(true|false)").FindStringSubmatch(inline[1])
			return match != nil && match[1] == "true"
		}
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
	pre, _, err := activationReadFile(path)
	if err != nil {
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
	post, exists, err := activationReadFile(path)
	if err != nil {
		return nil, err
	}
	if exists {
		if repaired, changed := multiAgentV2Preserve(string(pre), string(post), want); changed {
			if err := activationPublish(path, []byte(repaired)); err != nil {
				return nil, err
			}
		}
	}
	state := ReadMultiAgentV2State(deps)
	return &MultiAgentV2Change{state.Version, state.V2Enabled, true, state.MultiAgentV2Context}, nil
}
