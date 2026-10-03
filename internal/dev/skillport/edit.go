//go:build dev

package skillport

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
)

// splitLines is text as byte segments that keep their line terminator, so CRLF and the final
// newline survive a round trip.
func splitLines(text string) []string {
	lines := strings.SplitAfter(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// diffLines is the hunks that turn a into b: the common prefix and suffix are dropped and the middle
// is diffed by longest common subsequence (one hunk when the middle is too large for the table).
func diffLines(a, b []string) []Hunk {
	p, s := 0, 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	for s < len(a)-p && s < len(b)-p && a[len(a)-1-s] == b[len(b)-1-s] {
		s++
	}
	ma, mb := a[p:len(a)-s], b[p:len(b)-s]
	if len(ma) == 0 && len(mb) == 0 {
		return nil
	}
	if len(ma)*len(mb) > 1<<22 {
		return []Hunk{{Line: p + 1, Old: ma, New: mb}}
	}
	lcs := make([][]int, len(ma)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(mb)+1)
	}
	for i := len(ma) - 1; i >= 0; i-- {
		for j := len(mb) - 1; j >= 0; j-- {
			lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			if ma[i] == mb[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			}
		}
	}
	var hunks []Hunk
	open := false
	for i, j := 0, 0; i < len(ma) || j < len(mb); {
		if i < len(ma) && j < len(mb) && ma[i] == mb[j] {
			open = false
			i, j = i+1, j+1
			continue
		}
		if !open {
			hunks, open = append(hunks, Hunk{Line: p + i + 1}), true
		}
		h := &hunks[len(hunks)-1]
		if j < len(mb) && (i == len(ma) || lcs[i][j+1] >= lcs[i+1][j]) {
			h.New, j = append(h.New, mb[j]), j+1
		} else {
			h.Old, i = append(h.Old, ma[i]), i+1
		}
	}
	return hunks
}

// revert undoes hunks on the staged lines. Each hunk must be non-empty, ascending and clear of the
// one before, and the staged lines at its shifted position must be exactly its New lines; the
// restored lines must be whole lines again. Hunks are undone from the bottom, so positions hold.
func revert(staged []string, hunks []Hunk) ([]string, error) {
	at := make([]int, len(hunks))
	delta, floor := 0, 1
	for k, h := range hunks {
		if len(h.Old)+len(h.New) == 0 || slices.Equal(h.Old, h.New) {
			return nil, fmt.Errorf("hunk %d is empty or changes nothing", k+1)
		}
		if h.Line < floor {
			return nil, fmt.Errorf("hunk %d starts at line %d, before the end of the one above", k+1, h.Line)
		}
		at[k] = h.Line - 1 + delta
		if at[k] < 0 || at[k] > len(staged) || len(h.New) > len(staged)-at[k] || !slices.Equal(staged[at[k]:at[k]+len(h.New)], h.New) {
			return nil, fmt.Errorf("the staged lines at hunk %d (original line %d) are not its recorded new lines", k+1, h.Line)
		}
		delta, floor = delta+len(h.New)-len(h.Old), h.Line+len(h.Old)
	}
	out := slices.Clone(staged)
	for k := len(hunks) - 1; k >= 0; k-- {
		out = slices.Concat(out[:at[k]], hunks[k].Old, out[at[k]+len(hunks[k].New):])
	}
	if !slices.Equal(splitLines(strings.Join(out, "")), out) {
		return nil, errors.New("a recorded old line is not a whole original line")
	}
	return out, nil
}

// RecordEdits records the staged skill's differences from its substituted original as edits: hunks
// for a text file, an add for a file only the staged copy has, a remove for a dropped original. It
// refuses when the originals no longer match the record (it never re-baselines), a binary file
// differs or only an executable bit does. A file whose edit is unchanged keeps its reason; any
// other needs reason. It returns the number of edits recorded. Recording and staged-tree changes
// must be serialized by the operator; atomic replacement does not arbitrate concurrent updates.
func RecordEdits(root string, src Source, name, reason string) (int, error) {
	if !strings.HasPrefix(name, prefix) || !validFolder(strings.TrimPrefix(name, prefix)) {
		return 0, errors.New("not a staged skill name")
	}
	if err := layout(root); err != nil {
		return 0, err
	}
	skill, err := load(root, name)
	if err == nil {
		err = skill.validate(name)
	}
	if err != nil {
		return 0, err
	}
	sub, err := newSubstituter(root)
	if err != nil {
		return 0, err
	}
	if sub.isStub(skill.From) {
		return 0, fmt.Errorf("the name table no longer ports %s", skill.From)
	}
	if skill.Table != sub.digest {
		return 0, errors.New("name-substitution table changed since this skill was staged")
	}
	if got, err := Listing(src.skills()); err != nil || got != skill.Origin.SkillsListing || src.Origin != skill.Origin {
		return 0, fmt.Errorf("the source is not the origin the record names (listing %s, %v)", got, err)
	}
	orig, err := sub.render(filepath.Join(src.skills(), skill.From))
	if err != nil {
		return 0, err
	}
	for p, f := range orig {
		if e, ok := skill.Files[p]; !ok || e.Original != sum(f.data) || e.Exec != f.exec {
			return 0, fmt.Errorf("the record is stale against the source (%s): stage again", p)
		}
	}
	if len(orig) != len(skill.Files) {
		return 0, errors.New("the record is stale against the source (file set differs): stage again")
	}
	staged, err := readTree(filepath.Join(root, StagingRoot, name))
	if err != nil {
		return 0, err
	}
	prior := map[string]Edit{}
	for _, e := range skill.Edits {
		prior[e.File] = e
	}
	names := map[string]bool{}
	for p := range orig {
		names[p] = true
	}
	for p := range staged {
		names[p] = true
	}
	edits := []Edit{}
	for _, p := range slices.Sorted(maps.Keys(names)) {
		o, inOrig := orig[p]
		s, inStaged := staged[p]
		var e Edit
		switch {
		case !inOrig:
			e = Edit{File: p, Add: true, SHA256: sum(s.data), Exec: s.exec}
		case !inStaged:
			e = Edit{File: p, Remove: true}
		case o.exec != s.exec:
			return 0, fmt.Errorf("%s: only an added file carries an executable bit, a change of it cannot be recorded", p)
		case bytes.Equal(o.data, s.data):
			continue
		case !isText(o.data) || !isText(s.data):
			return 0, fmt.Errorf("%s: binary content differs and cannot be recorded", p)
		default:
			e = Edit{File: p, Hunks: diffLines(splitLines(string(o.data)), splitLines(string(s.data)))}
		}
		if before := prior[p]; before.Reason != "" && reflect.DeepEqual(before, withReason(e, before.Reason)) {
			e.Reason = before.Reason
		} else if e.Reason = reason; strings.TrimSpace(reason) == "" {
			return 0, fmt.Errorf("%s: a reason is required for a new or changed edit", p)
		}
		edits = append(edits, e)
	}
	skill.Edits = edits
	if err := skill.validate(name); err != nil {
		return 0, err
	}
	if problems := compare(StagingRoot+"/"+name, skill, staged); len(problems) > 0 {
		return 0, errors.New(strings.Join(problems, "\n"))
	}
	return len(edits), save(root, name, skill)
}

func withReason(e Edit, reason string) Edit {
	e.Reason = reason
	return e
}
