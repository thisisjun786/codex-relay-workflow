package bundle

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

var hunk = regexp.MustCompile(`(?m)^@@ -[0-9]+(?:,[0-9]+)? \+([0-9]+)(?:,([0-9]+))? @@`)

func (g gitRepo) headText(f *file, o Options) error {
	f.Binary = f.Binary || strings.ContainsRune(f.patch, 0) || !utf8.ValidString(f.patch)
	if f.Status == "D" || f.Mode == "160000" || f.Binary {
		return nil
	}
	out, err := g.run(nil, "show", "--no-ext-diff", "--no-textconv", g.head+":"+f.Path)
	if err != nil {
		return err
	}
	f.HeadBytes = len(out)
	text := string(out)
	if strings.ContainsRune(text, 0) || !utf8.Valid(out) {
		f.Binary = true
		return nil
	}
	lines := textLines(text)
	f.HeadLines = len(lines)
	f.text = numbered(lines, nil, o.MaxFileBytes)
	full := numbered(lines, nil, len(text)+len(lines)*32)
	f.Truncated = f.text != full
	if f.Truncated {
		wanted := make(map[int]bool)
		for _, m := range hunk.FindAllStringSubmatch(f.patch, -1) {
			start, _ := strconv.Atoi(m[1])
			n := 1
			if m[2] != "" {
				n, _ = strconv.Atoi(m[2])
			}
			for line := max(1, start); line <= min(len(lines), start+max(n, 1)-1); line++ {
				wanted[line] = true
			}
		}
		if len(wanted) == 0 {
			for n := 1; n <= min(len(lines), o.Context); n++ {
				wanted[n] = true
			}
		}
		f.text = numbered(lines, wanted, o.MaxFileBytes)
	}
	f.TextBytes = len(f.text)
	return nil
}

func textLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// numbered returns at most cap bytes, retaining an incomplete line's UTF-8 prefix.
func numbered(lines []string, wanted map[int]bool, cap int) string {
	var b strings.Builder
	for i, line := range lines {
		if wanted != nil && !wanted[i+1] {
			continue
		}
		s := fmt.Sprintf("%d| %s\n", i+1, line)
		remaining := cap - b.Len()
		if len(s) > remaining {
			s = s[:remaining]
			for !utf8.ValidString(s) {
				s = s[:len(s)-1]
			}
			b.WriteString(s)
			break
		}
		b.WriteString(s)
	}
	return b.String()
}

func (g gitRepo) rules(cap int, m *Metadata) (string, error) {
	var b strings.Builder
	for i, path := range []string{"AGENTS.md", "POLICY.md", "CONTRIBUTING.md"} {
		r := RuleMetadata{Path: path}
		entry, err := g.run(nil, "ls-tree", "-z", g.head, "--", path)
		if err != nil {
			return "", err
		}
		if len(entry) == 0 {
			r.Missing = true
			m.Rules = append(m.Rules, r)
			continue
		}
		if !strings.Contains(string(entry), " blob ") {
			return "", fmt.Errorf("rule %s is not a blob", path)
		}
		out, err := g.run(nil, "show", "--no-ext-diff", "--no-textconv", g.head+":"+path)
		if err != nil {
			return "", err
		}
		share := cap / 3
		if i < cap%3 {
			share++
		}
		lines := textLines(strings.ToValidUTF8(string(out), "�"))
		body := numbered(lines, nil, share)
		r.SourceBytes, r.Bytes = len(out), len(body)
		r.Truncated = body != numbered(lines, nil, len(out)+len(lines)*32)
		m.Rules = append(m.Rules, r)
		m.RuleBytes += r.Bytes
		b.WriteString(part("RULE", path, body, r.Truncated))
	}
	return b.String(), nil
}
