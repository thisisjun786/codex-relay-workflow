// Package layername is how relay text names a session layer, and the one check that finds a relay
// role id written where a person reads a layer. The layers and their names are the table of
// plugins/crw/skills/crw-canon/references/hierarchy.md "The levels": the relay role id `supervisor`
// is the 부모 (the repository session), the relay role id `parent` is the 자식 (the project session),
// and the relay role id `child` is named by its id, a `child` session, which is no layer. The role ids
// stay as they are in stored rows, flags, wire fields and refusal codes; the text a person reads names
// the layer (CRW-1432).
//
// Name, Gloss, Typed and ListGloss build that text. Find is the matcher the ci ratchet
// (internal/dev/ci role_words_test.go) and the package tests share, so both count the same words.
// The package imports the standard library only, so any relay package can use it.
package layername

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Name is the layer name a person reads for a relay role id: supervisor is 부모, parent is 자식,
// child is "`child` session". Any other id is returned unchanged.
func Name(roleID string) string {
	switch roleID {
	case "supervisor":
		return "부모"
	case "parent":
		return "자식"
	case "child":
		return "`child` session"
	}
	return roleID
}

// Gloss is the layer name in parentheses with a leading space, written after a role id that is shown
// as a value (" (자식)" after 'parent'); "" for an id that is no relay role.
func Gloss(roleID string) string {
	if name := Name(roleID); name != roleID {
		return " (" + name + ")"
	}
	return ""
}

// Typed is a role id where a person types or reads it as a value, with its layer name:
// "supervisor (부모)", "parent (자식)", "child (`child` session)".
func Typed(roleID string) string { return roleID + Gloss(roleID) }

// ListGloss is the layer names of a list of role ids in the given order, written after the list's
// own spelling: " (`child` session, 자식)" for ["child", "parent"]; "" for an empty list.
func ListGloss(roleIDs []string) string {
	if len(roleIDs) == 0 {
		return ""
	}
	names := make([]string, len(roleIDs))
	for i, id := range roleIDs {
		names[i] = Name(id)
	}
	return " (" + strings.Join(names, ", ") + ")"
}

// Hit is one bare role word in a piece of human text: the word as written (a trailing 's included)
// and its byte offset.
type Hit struct {
	Word   string
	Offset int
}

// roleWords are the flagged words, longest first so a plural is matched before its singular.
var roleWords = []string{"supervisors", "supervisor", "parents", "parent"}

// Find is every bare role word in text, in order: parent, parents, supervisor or supervisors in any
// case, a trailing 's counted as the same hit. A word counts only on its own: the character before
// it is not a letter, digit, '_', '-' or '=', and the one after it is not a letter, digit, '_', '='
// or a '.' followed by a letter (a field path). A following '-' ends the word (parent-authored is a
// hit) except in parent-class, a decision class, and parent-title, a skill command.
//
// A command name that 1.0 keeps is no layer (CRW-1477): the supervisor or parent that begins the
// command word right after "crw <family> " (crw manage supervisor register, crw relay supervisor-stage),
// where crw stands alone, family is one of crwFamilies and the whole command token is one of the
// family's commandNames (CRW-1546), is not counted. The running text around such a command is, and so
// is a token that is no command of the family (crw manage the supervisor, crw manage supervisor's seat,
// crw relay supervisor-authored, the supervisor section).
//
// Not counted: a typed role id (supervisor (부모), parent (자식)); a quoted role id followed by its
// layer name ('parent' (자식), "parent" (자식)); a quoted role list followed by its ListGloss
// (['child', 'parent'] (`child` session, 자식), ["child","parent"] (`child` session, 자식)); a flag value
// (--role supervisor); and the git senses first parent, second parent, parent revision(s), parent
// commit(s), parent %d and has %d parent(s). A wrong layer name after a role id is a hit. A possessive
// names a layer's own thing, so it is a hit after a flag and before revision, commit or %d (the
// parent's revision, --role supervisor's); only first and second parent's stay git senses.
func Find(text string) []Hit {
	glossed := glossedLists(text)
	var hits []Hit
	for i := 0; i < len(text); i++ {
		c := text[i] | 0x20
		if c != 'p' && c != 's' {
			continue
		}
		word := matchWord(text, i)
		if word == "" {
			continue
		}
		end := i + len(word)
		if !boundaryBefore(text, i) {
			i = end - 1
			continue
		}
		if strings.HasPrefix(text[end:], "'s") && !wordChar(runeAt(text, end+2)) {
			end += 2
		}
		if !boundaryAfter(text, end) || covered(glossed, i) || exempt(text, i, word, end) {
			i = end - 1
			continue
		}
		hits = append(hits, Hit{Word: text[i:end], Offset: i})
		i = end - 1
	}
	return hits
}

// matchWord is the flagged word that starts at text[i] (ASCII, any case), or "".
func matchWord(text string, i int) string {
	for _, w := range roleWords {
		if len(text)-i >= len(w) && asciiFold(text[i:i+len(w)], w) {
			return text[i : i+len(w)]
		}
	}
	return ""
}

// asciiFold is whether s equals the lower-case ASCII word w in any ASCII case.
func asciiFold(s, w string) bool {
	for k := 0; k < len(w); k++ {
		c := s[k]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != w[k] {
			return false
		}
	}
	return true
}

// wordChar is a letter, a digit or '_'.
func wordChar(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

// runeAt is the rune that starts at text[i], or utf8.RuneError past the end.
func runeAt(text string, i int) rune {
	if i >= len(text) {
		return utf8.RuneError
	}
	r, _ := utf8.DecodeRuneInString(text[i:])
	return r
}

func boundaryBefore(text string, i int) bool {
	if i == 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(text[:i])
	return !wordChar(r) && r != '-' && r != '='
}

func boundaryAfter(text string, end int) bool {
	if end >= len(text) {
		return true
	}
	r := runeAt(text, end)
	if wordChar(r) || r == '=' {
		return false
	}
	return !(r == '.' && unicode.IsLetter(runeAt(text, end+1)))
}

// exempt is whether the word text[i:end] (word without a trailing 's) is a form Find does not count.
// Only first and second parent hold with a trailing 's: the other forms are a bare id, a flag value or
// a git noun phrase, and a possessive is none of them.
func exempt(text string, i int, word string, end int) bool {
	lower := strings.ToLower(word)
	rest := text[end:]
	possessive := end != i+len(word)
	if (lower == "parent" || lower == "parents") && (precededByWord(text[:i], "first") || precededByWord(text[:i], "second")) {
		return true
	}
	if possessive {
		return false
	}
	// A typed role id: the exact id followed by its own layer name.
	if g := Gloss(word); g != "" && strings.HasPrefix(rest, g) {
		return true
	}
	// A quoted role id followed by its own layer name.
	if g := Gloss(word); g != "" && i > 0 {
		if q := text[i-1]; (q == '\'' || q == '"') && strings.HasPrefix(rest, string(q)+g) {
			return true
		}
	}
	if flagValue(text[:i]) {
		return true
	}
	if family := commandWord(text[:i]); family != "" && commandNames[family][commandToken(text[i:])] {
		return true
	}
	if lower == "parent" && (hasWordPrefix(rest, "-class") || hasWordPrefix(rest, "-title")) {
		return true
	}
	if lower != "parent" && lower != "parents" {
		return false
	}
	for _, next := range []string{" revisions", " revision", " commits", " commit"} {
		if hasWordPrefix(rest, next) {
			return true
		}
	}
	if strings.HasPrefix(rest, " %d") {
		return true
	}
	before := strings.ToLower(text[:i])
	return strings.HasSuffix(before, "has %d ") && (before == "has %d " || !wordChar(lastRune(before[:len(before)-len("has %d ")])))
}

// crwFamilies are the crw command families that take a command word (cmd/crw's listed modes
// without help and version).
var crwFamilies = map[string]bool{
	"relay": true, "bridge": true, "hook": true, "skill": true, "doctor": true, "install": true,
	"review": true, "manage": true, "gui": true, "config": true, "tools": true, "recall": true,
	"pabcd": true, "role": true, "provider": true, "map": true, "flow": true,
}

// commandNames are, per crw family, the command names 1.0 keeps that begin with supervisor or parent:
// the only command tokens Find does not count after "crw <family> " (CRW-1546). The relay set is every
// registered relay command so named (internal/relay/cli command_words_test.go keeps the two equal).
var commandNames = map[string]map[string]bool{
	"relay": {
		"supervisor-standing": true, "supervisor-select": true, "supervisor-report-recorded": true,
		"supervisor-stage": true, "supervisor-show": true, "supervisor-list": true,
		"supervisor-acknowledge": true, "supervisor-send": true, "supervisor-read": true,
	},
	"manage": {"supervisor": true},
	"skill":  {"parent-title": true},
}

// CommandNames is the family's command names that begin with supervisor or parent, sorted; nil for a
// family that has none.
func CommandNames(family string) []string {
	var names []string
	for name := range commandNames[family] {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// commandWord is the family when before ends with "crw <family> ", crw standing alone (not after a
// letter, digit, '_' or '-') and family one of crwFamilies, so the word that follows is the family's
// command; "" otherwise.
func commandWord(before string) string {
	if !strings.HasSuffix(before, " ") {
		return ""
	}
	name := before[:len(before)-1]
	k := strings.LastIndexByte(name, ' ')
	if k < 0 || !crwFamilies[name[k+1:]] || !strings.HasSuffix(name[:k], "crw") {
		return ""
	}
	rest := name[:k-len("crw")]
	if rest != "" {
		if r := lastRune(rest); wordChar(r) || r == '-' {
			return ""
		}
	}
	return name[k+1:]
}

// commandToken is the command token that starts text: up to the first rune that is neither a letter,
// a digit, '_' nor '-'.
func commandToken(text string) string {
	end := strings.IndexFunc(text, func(r rune) bool { return !wordChar(r) && r != '-' })
	if end < 0 {
		return text
	}
	return text[:end]
}

// hasWordPrefix is whether rest starts with next (any ASCII case) and next ends a word there.
func hasWordPrefix(rest, next string) bool {
	return len(rest) >= len(next) && asciiFold(rest[:len(next)], next) && !wordChar(runeAt(rest, len(next)))
}

// precededByWord is whether before ends with word (any ASCII case) and one space, word standing alone.
func precededByWord(before, word string) bool {
	n := len(word) + 1
	if len(before) < n || before[len(before)-1] != ' ' || !asciiFold(before[len(before)-n:len(before)-1], word) {
		return false
	}
	return len(before) == n || !wordChar(lastRune(before[:len(before)-n]))
}

func lastRune(s string) rune {
	r, _ := utf8.DecodeLastRuneInString(s)
	return r
}

// flagValue is whether before ends with a flag name and one space ("--role "), so the word that
// follows is the flag's value. (A value after "--role=" is already no hit: '=' is no boundary.)
func flagValue(before string) bool {
	if !strings.HasSuffix(before, " ") {
		return false
	}
	name := before[:len(before)-1]
	k := len(name)
	for k > 0 {
		c := name[k-1]
		if c == '-' || c == '_' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9') {
			k--
			continue
		}
		break
	}
	flag := name[k:]
	if !strings.HasPrefix(flag, "--") || len(flag) == 2 || flag[2] == '-' {
		return false
	}
	return k == 0 || !wordChar(lastRune(name[:k]))
}

// span is a byte range [start, end) of text.
type span struct{ start, end int }

func covered(spans []span, i int) bool {
	for _, s := range spans {
		if s.start <= i && i < s.end {
			return true
		}
	}
	return false
}

// glossedLists are the role lists in text followed directly by their ListGloss: '[', quoted ids
// (the same quote on both sides) separated by ',' and an optional space, ']', then " (" and the
// layer names of the ids in order joined by ", ", then ')'. The words inside such a list are no hit.
func glossedLists(text string) []span {
	var spans []span
	for start := strings.IndexByte(text, '['); start >= 0; {
		ids, end, ok := parseList(text, start)
		if ok && len(ids) > 0 && strings.HasPrefix(text[end:], ListGloss(ids)) {
			spans = append(spans, span{start, end})
		}
		next := strings.IndexByte(text[start+1:], '[')
		if next < 0 {
			break
		}
		start += 1 + next
	}
	return spans
}

// parseList reads a list of quoted ids at text[start] == '['. It returns the ids and the offset
// just past ']'.
func parseList(text string, start int) ([]string, int, bool) {
	i := start + 1
	var ids []string
	for {
		if i >= len(text) || (text[i] != '\'' && text[i] != '"') {
			return nil, 0, false
		}
		q := text[i]
		j := i + 1
		for j < len(text) && idChar(text[j]) {
			j++
		}
		if j == i+1 || j >= len(text) || text[j] != q {
			return nil, 0, false
		}
		ids = append(ids, text[i+1:j])
		i = j + 1
		if i < len(text) && text[i] == ']' {
			return ids, i + 1, true
		}
		if i >= len(text) || text[i] != ',' {
			return nil, 0, false
		}
		i++
		if i < len(text) && text[i] == ' ' {
			i++
		}
	}
}

// idChar is a character of a role id: an ASCII letter, digit, '_' or '-'.
func idChar(c byte) bool {
	return c == '_' || c == '-' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9')
}
