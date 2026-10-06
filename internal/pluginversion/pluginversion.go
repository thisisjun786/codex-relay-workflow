// Package pluginversion holds the rules that name a plugin payload's version: which files a package
// ships, how they are digested, and how the manifest records that digest as the version's suffix.
//
// crw-dev ci plugin (internal/dev/ci) and crw skill base-refresh mechanical (internal/skill) read the
// same rules from here, so a version recorded in the manifest and a version recomputed from a commit
// cannot drift apart.
package pluginversion

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"unicode/utf8"
)

// The package rules: where the plugin package is and where its manifest lives.
const (
	// PluginRelative is the plugin package's directory in the repository.
	PluginRelative = "plugins/crw"
	// ManifestPath is the plugin manifest's path inside the package.
	ManifestPath = ".codex-plugin/plugin.json"
	// ManifestRepoPath is the plugin manifest's path from the repository root.
	ManifestRepoPath = PluginRelative + "/" + ManifestPath
	// PayloadSuffixLength is how many hex digits of the payload digest the version's suffix carries.
	PayloadSuffixLength = 12
)

// errNotUTF8 is a document that is not UTF-8 text.
var errNotUTF8 = errors.New("not UTF-8 text")

// Entry is one shipped file: its git mode and its bytes.
type Entry struct {
	Mode string
	Data []byte
}

// Payload is the files a package ships, by package-relative name.
type Payload map[string]Entry

// Names is the payload's names in order.
func (p Payload) Names() []string {
	names := make([]string, 0, len(p))
	for name := range p {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Has reports whether the payload ships name.
func (p Payload) Has(name string) bool {
	_, ok := p[name]
	return ok
}

// Digest is sha256 over length-framed "<len>:<name> <mode> <sha256>" lines in name order. Its first
// twelve hex digits are the version suffix the manifest records, so these bytes are fixed.
func Digest(p Payload) string {
	var blocks [][]byte
	for _, name := range p.Names() {
		sum := sha256.Sum256(p[name].Data)
		blocks = append(blocks, fmt.Appendf(nil, "%d:%s %s %s", len(name), name, p[name].Mode, hex.EncodeToString(sum[:])))
	}
	sum := sha256.Sum256(bytes.Join(blocks, []byte("\n")))
	return hex.EncodeToString(sum[:])
}

// SplitVersion is a version's release and its suffix, "" when it has none.
func SplitVersion(version string) (string, string) {
	release, suffix, _ := strings.Cut(version, "+")
	return release, suffix
}

// VersionPayload is the payload with the manifest's recorded suffix elided, so recording the digest
// does not change it.
func VersionPayload(p Payload, version string) (Payload, error) {
	release, suffix := SplitVersion(version)
	if suffix == "" || !p.Has(ManifestPath) {
		return p, nil
	}
	manifest := p[ManifestPath]
	recorded, plain := []byte(show(version)), []byte(show(release))
	if count := bytes.Count(manifest.Data, recorded); count != 1 {
		return nil, fmt.Errorf("%s spells %q %d times; the suffix has to be elided exactly once for the digest beneath it to be derived at all",
			ManifestPath, version, count)
	}
	out := maps.Clone(p)
	out[ManifestPath] = Entry{manifest.Mode, bytes.ReplaceAll(manifest.Data, recorded, plain)}
	return out, nil
}

// PayloadVersion is the version that names p: the release it records plus the digest of the payload
// with that recorded suffix elided.
func PayloadVersion(p Payload, version string) (string, error) {
	release, _ := SplitVersion(version)
	elided, err := VersionPayload(p, version)
	if err != nil {
		return "", err
	}
	return release + "+" + Digest(elided)[:PayloadSuffixLength], nil
}

// ManifestVersion is the version the payload's manifest records.
func ManifestVersion(p Payload) (string, error) {
	manifest, ok := p[ManifestPath]
	if !ok {
		return "", fmt.Errorf("%s is missing from the package", ManifestPath)
	}
	m, err := decodeObject(manifest.Data)
	if err != nil {
		return "", fmt.Errorf("%s: %w", ManifestPath, err)
	}
	return versionOf(m)
}

// ManifestVersionElided is the manifest's bytes with the version it records replaced by an empty
// string, and that version. Two manifests with the same elided bytes differ only in the version.
func ManifestVersionElided(data []byte) ([]byte, string, error) {
	m, err := decodeObject(data)
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", ManifestPath, err)
	}
	version, err := versionOf(m)
	if err != nil {
		return nil, "", err
	}
	recorded := []byte(show(version))
	if count := bytes.Count(data, recorded); count != 1 {
		return nil, "", fmt.Errorf("%s spells %q %d times; the version has to stand exactly once for two manifests to be compared apart from it",
			ManifestPath, version, count)
	}
	return bytes.ReplaceAll(data, recorded, []byte("\"\"")), version, nil
}

// versionOf is the version a manifest object records.
func versionOf(m map[string]any) (string, error) {
	version, ok := m["version"].(string)
	if !ok || version == "" {
		return "", fmt.Errorf("%s records no version, and a version is what names a payload", ManifestPath)
	}
	return version, nil
}

// decodeObject is the JSON object a document holds: UTF-8 text, one value and nothing after it.
func decodeObject(data []byte) (map[string]any, error) {
	if !utf8.Valid(data) {
		return nil, errNotUTF8
	}
	if err := json.Unmarshal(data, new(json.RawMessage)); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	m, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("the manifest must be a JSON object")
	}
	return m, nil
}

// show is a value as a message names it: its compact JSON text.
func show(value any) string {
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return fmt.Sprint(value)
	}
	return strings.TrimSuffix(b.String(), "\n")
}
