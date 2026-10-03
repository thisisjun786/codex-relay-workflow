//go:build dev

// Package skillport stages CXC v0.2.40 skills outside the plugin root and checks the staged copies.
//
// "skillport stage" copies a skill folder of the extracted CXC tree into port/cxc/skills/crw-<folder>
// with the checked-in name table applied, as the corpus replay applies it, and records the digest of
// every substituted original file in port/cxc/records/crw-<folder>.json. "skillport check" verifies the
// staged tree against the records; crw-dev ci validate runs the same Check and validates the staged
// skills like the plugin's own. Offline, Check proves each staged file is what its record holds under
// the pinned name table; any difference is refused (recording hand edits is a later change). It holds
// no original: that the digests are the substituted originals is proved by check --source against an
// extracted tree. Stage never overwrites; to refresh a skill, remove its directory and record with git.
package skillport

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/cxccorpus"
)

const (
	StagingRoot = "port/cxc/skills"
	RecordDir   = "port/cxc/records"
	prefix      = "crw-"
)

// Origin names the original tree: the tag, its commit and the digest of the original skills
// directory listing.
type Origin struct {
	Tag           string `json:"tag"`
	Commit        string `json:"commit"`
	SkillsListing string `json:"skills_listing_sha256"`
}

// Source is an extracted CXC tree and the origin it must have.
type Source struct {
	Dir    string
	Origin Origin
}

func (s Source) skills() string { return filepath.Join(s.Dir, "plugins/codexclaw/skills") }

// DefaultOrigin is CXC v0.2.40. The listing digest is one line per regular file in byte order, the
// sha256sum line with " x " as the separator of an executable file, hashed:
// (cd <tree>/plugins/codexclaw/skills && find . -type f -print0 | LC_ALL=C sort -z | xargs -0 sh -c
// 'for f; do s="  "; [ -x "$f" ] && s=" x "; printf "%s%s%s\n" "$(sha256sum <"$f" | cut -d" " -f1)" "$s" "$f"; done' sh) | sha256sum
func DefaultOrigin() Origin {
	return Origin{"v0.2.40", "3c1459acadeb1906d97c00a598e1457327ae372d", "79d9580f39ca27221cb9c24337080e3fe4a1e71f76338c7329fca63f76a32e89"}
}

// FileEntry is an original file after the substitution: its digest and executable bit.
type FileEntry struct {
	Original string `json:"original"`
	Exec     bool   `json:"exec,omitempty"`
}

// Skill is the record of one staged skill.
type Skill struct {
	Origin Origin               `json:"origin"`
	Table  string               `json:"table_sha256"`
	From   string               `json:"from"`
	Files  map[string]FileEntry `json:"files"`
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func recordPath(root, name string) string { return filepath.Join(root, RecordDir, name+".json") }

func validFolder(s string) bool {
	return s != "" && s[0] != '-' && strings.Trim(s, "abcdefghijklmnopqrstuvwxyz0123456789-") == ""
}

func localPath(p string) bool {
	return filepath.IsLocal(p) && path.Clean(p) == p && !strings.Contains(p, "\\")
}

// layout refuses a staging root or records root that is, or lies below, anything but a plain
// directory (a symlink would carry reads and writes out of the checkout); a missing one is fine.
func layout(root string) error {
	for _, rel := range []string{StagingRoot, RecordDir} {
		p := root
		for _, part := range strings.Split(rel, "/") {
			p = filepath.Join(p, part)
			info, err := os.Lstat(p)
			if errors.Is(err, fs.ErrNotExist) {
				break
			}
			if err != nil || !info.IsDir() {
				return fmt.Errorf("%s is not a plain directory (%v)", p, err)
			}
		}
	}
	return nil
}

func load(root, name string) (*Skill, error) {
	if info, err := os.Lstat(recordPath(root, name)); err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("the record is not a regular file (%v)", err)
	}
	raw, err := os.ReadFile(recordPath(root, name))
	if err != nil {
		return nil, err
	}
	var s Skill
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after the JSON value")
	}
	return &s, nil
}

func encode(s *Skill) ([]byte, error) { return cxccorpus.Marshal(s) }

// writeTemp writes data to a new file next to target and returns its name.
func writeTemp(target string, data []byte) (string, error) {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(filepath.Dir(target), ".record-")
	if err != nil {
		return "", err
	}
	_, err = f.Write(data)
	if err = errors.Join(err, f.Close(), os.Chmod(f.Name(), 0o644)); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// validate refuses a record whose names cannot be trusted as paths.
func (s *Skill) validate(name string) error {
	if !validFolder(s.From) || name != prefix+s.From {
		return fmt.Errorf("record name %q does not match its folder %q", name, s.From)
	}
	for p := range s.Files {
		if !localPath(p) {
			return fmt.Errorf("file name %q is not a clean relative slash path", p)
		}
	}
	return nil
}
