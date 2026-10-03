//go:build dev

package skillport

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/cxccorpus"
)

type file struct {
	data []byte
	exec bool
}

func isText(b []byte) bool { return utf8.Valid(b) && !bytes.Contains(b, []byte{0}) }

// readTree is the regular files below dir by slash path; any other kind of entry is refused.
func readTree(dir string) (map[string]file, error) {
	files := map[string]file{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: not a regular file", filepath.ToSlash(rel))
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		files[filepath.ToSlash(rel)] = file{data, err == nil && info.Mode()&0o111 != 0}
		return nil
	})
	return files, err
}

// Listing is the digest of the sha256sum listing of the regular files below dir, in byte order, with
// " x " in place of the second space of the separator for an executable file (see DefaultOrigin).
func Listing(dir string) (string, error) {
	files, err := readTree(dir)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, name := range slices.Sorted(maps.Keys(files)) {
		sep := "  "
		if files[name].exec {
			sep = " x "
		}
		fmt.Fprintf(h, "%s%s./%s\n", sum(files[name].data), sep, name)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// substituter applies the name table as the corpus replayer does (internal/contracttest/cxc_replay.go):
// text a rewrite rule names and the upstream addresses of the never list keep their spelling.
type substituter struct {
	sub    *cxccorpus.Substituter
	mask   *regexp.Regexp
	digest string // of what rendering reads from the table
}

func newSubstituter(root string) (*substituter, error) {
	sub, err := cxccorpus.LoadSubstitution(root)
	if err != nil {
		return nil, err
	}
	s := &substituter{sub: sub}
	var keep []string
	for _, rule := range sub.File.Rules {
		if rule.Kind == "rewrite" {
			keep = append(keep, "(?:"+rule.Regex+")")
		}
	}
	var never []string
	for _, entry := range sub.File.Never {
		never = append(never, entry.Text)
		for _, token := range strings.Split(entry.Text, ", ") {
			if strings.Contains(token, "codexclaw") && strings.Contains(token, "/") && !strings.ContainsAny(token, " ()*") {
				keep = append(keep, regexp.QuoteMeta(token))
			}
		}
	}
	if len(keep) > 0 {
		if s.mask, err = regexp.Compile(strings.Join(keep, "|")); err != nil {
			return nil, err
		}
	}
	rules, cli := slices.Clone(sub.File.Rules), slices.Clone(sub.File.CLI)
	for i := range rules {
		rules[i].Note, rules[i].Site = "", ""
	}
	for i := range cli {
		cli[i].Note = ""
	}
	raw, err := cxccorpus.Marshal(struct {
		Rules []cxccorpus.SubstitutionRule
		CLI   []cxccorpus.CLIRename
		Never []string
	}{rules, cli, never})
	s.digest = sum(raw)
	return s, err
}

// rename is the substituted text. The placeholder holds NUL, which text never contains.
func (s *substituter) rename(text string) string {
	var kept []string
	if s.mask != nil {
		text = s.mask.ReplaceAllStringFunc(text, func(m string) string {
			kept = append(kept, m)
			return "\x00" + strconv.Itoa(len(kept)-1) + "\x00"
		})
	}
	parts := strings.Split(s.sub.Expected(text), "\x00")
	for i := 1; i < len(parts); i += 2 {
		n, _ := strconv.Atoi(parts[i])
		parts[i] = kept[n]
	}
	return strings.Join(parts, "")
}

// isStub is a folder the table does not port (a redirect stub or an out-of-scope skill).
func (s *substituter) isStub(folder string) bool {
	return slices.ContainsFunc(s.sub.File.Skills, func(k cxccorpus.SkillRename) bool { return k.CXC == "cxc-"+folder && k.CRW == "" })
}

// render is the substituted original of the skill directory dir. A text that still holds the
// replay placeholder {CRW} (rule R27, a resolver) is refused.
func (s *substituter) render(dir string) (map[string]file, error) {
	files, err := readTree(dir)
	for p, f := range files {
		if isText(f.data) {
			f.data = []byte(s.rename(string(f.data)))
			if bytes.Contains(f.data, []byte("{CRW}")) {
				return nil, fmt.Errorf("%s: the substituted text holds the replay placeholder {CRW} (rule R27)", p)
			}
			files[p] = f
		}
	}
	return files, err
}
