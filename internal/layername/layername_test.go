package layername

import (
	"reflect"
	"testing"
)

// TestLayernameMapsRoleIds pins the role id to layer name table of hierarchy.md "The levels":
// supervisor is 부모, parent is 자식, child is an issue child, and any other id is left as it is.
func TestLayernameMapsRoleIds(t *testing.T) {
	cases := []struct {
		id, name, gloss, typed string
	}{
		{"supervisor", "부모", " (부모)", "supervisor (부모)"},
		{"parent", "자식", " (자식)", "parent (자식)"},
		{"child", "issue child", " (issue child)", "child (issue child)"},
		{"helper", "helper", "", "helper"},
		{"", "", "", ""},
		{"Parent", "Parent", "", "Parent"},
	}
	for _, c := range cases {
		if got := Name(c.id); got != c.name {
			t.Errorf("Name(%q) = %q, want %q", c.id, got, c.name)
		}
		if got := Gloss(c.id); got != c.gloss {
			t.Errorf("Gloss(%q) = %q, want %q", c.id, got, c.gloss)
		}
		if got := Typed(c.id); got != c.typed {
			t.Errorf("Typed(%q) = %q, want %q", c.id, got, c.typed)
		}
	}
	lists := []struct {
		ids  []string
		want string
	}{
		{[]string{"child", "parent"}, " (issue child, 자식)"},
		{[]string{"parent", "child"}, " (자식, issue child)"},
		{[]string{"supervisor"}, " (부모)"},
		{[]string{"supervisor", "helper"}, " (부모, helper)"},
		{[]string{}, ""},
		{nil, ""},
	}
	for _, c := range lists {
		if got := ListGloss(c.ids); got != c.want {
			t.Errorf("ListGloss(%q) = %q, want %q", c.ids, got, c.want)
		}
	}
}

// TestLayernameFindsBareRoleWords is the D4 table of plan r4 (CRW-1432): the bare role words a
// person would read as a layer, and the forms that are values, flags, identifiers or git senses.
func TestLayernameFindsBareRoleWords(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []Hit
	}{
		// Hits.
		{"ask the parent", "ask the parent", []Hit{{"parent", 8}}},
		{"store-scope supervisor", "the store-scope supervisor", []Hit{{"supervisor", 16}}},
		{"capitalised", "Parent", []Hit{{"Parent", 0}}},
		{"bare word (the ci scan skips a literal without whitespace, Find does not)", "parent", []Hit{{"parent", 0}}},
		{"upper case", "THE SUPERVISOR", []Hit{{"SUPERVISOR", 4}}},
		{"plurals", "parents and supervisors", []Hit{{"parents", 0}, {"supervisors", 12}}},
		{"possessive is one hit", "the parent's turn", []Hit{{"parent's", 4}}},
		{"plural possessive", "the parents' turn", []Hit{{"parents", 4}}},
		{"parent-authored", "a parent-authored note", []Hit{{"parent", 2}}},
		{"parent-made", "a parent-made refresh", []Hit{{"parent", 2}}},
		{"sentence end", "send it to the parent.", []Hit{{"parent", 15}}},
		{"quoted without gloss", "is bound as 'supervisor' and", []Hit{{"supervisor", 13}}},
		{"role quoted without gloss", "role 'supervisor' cannot declare", []Hit{{"supervisor", 6}}},
		{"double quoted without gloss", `role "parent" here`, []Hit{{"parent", 6}}},
		{"wrong typed pair", "parent (부모)", []Hit{{"parent", 0}}},
		{"wrong quoted pair", "'supervisor' (자식)", []Hit{{"supervisor", 1}}},
		{"typed plural is not typed", "supervisors (부모)", []Hit{{"supervisors", 0}}},
		{"capitalised typed is not typed", "Parent (자식)", []Hit{{"Parent", 0}}},
		{"list without gloss", "holds live bindings at ['child', 'parent']; one", []Hit{{"parent", 34}}},
		{"list with reordered gloss", "holds live bindings at ['child', 'parent'] (자식, issue child); one", []Hit{{"parent", 34}}},
		{"list with missing name", "at ['child', 'parent'] (issue child); one", []Hit{{"parent", 14}}},
		{"list with wrong name", "at ['parent'] (부모)", []Hit{{"parent", 5}}},
		{"list with mixed quotes", `at ['parent"] (자식)`, []Hit{{"parent", 5}}},
		{"list with gloss after a gap", "at ['parent']  (자식)", []Hit{{"parent", 5}}},
		{"list of plain words", "at [parent] (자식)", []Hit{{"parent", 4}}},
		{"flag named before the word", "the --role flag names the parent", []Hit{{"parent", 26}}},
		{"after a colon", "to: parent ", []Hit{{"parent", 4}}},
		{"after Korean text", "자식 parent", []Hit{{"parent", 7}}},
		{"two in one text", "the parent asks the supervisor", []Hit{{"parent", 4}, {"supervisor", 20}}},
		{"git sense needs its word", "the third parent", []Hit{{"parent", 10}}},
		{"has without count", "has parent(s)", []Hit{{"parent", 4}}},
		// A possessive is a layer's: the git senses and the flag value hold only for the bare word.
		{"possessive before revision", "the parent's revision", []Hit{{"parent's", 4}}},
		{"possessive before commit", "the parent's commit", []Hit{{"parent's", 4}}},
		{"possessive before a count", "the parent's %d", []Hit{{"parent's", 4}}},
		{"possessive flag value", "--role supervisor's", []Hit{{"supervisor's", 7}}},

		// Not hits.
		{"child is never a hit", "the child and its children", nil},
		{"flag value", "--role supervisor", nil},
		{"flag value with equals", "--role=parent", nil},
		{"flag value in a command", "crw relay linkage-handover --role supervisor --actor parent", nil},
		{"authority value", "--authority parent=ref", nil},
		{"assignment", "parent=ref", nil},
		{"typed supervisor", "supervisor (부모)", nil},
		{"typed parent", "the parent (자식) authority", nil},
		{"quoted glossed parent", "'parent' (자식)", nil},
		{"quoted glossed supervisor", "is bound as 'supervisor' (부모) and", nil},
		{"double quoted glossed parent", `"parent" (자식)`, nil},
		{"glossed list repr", "holds live bindings at ['child', 'parent'] (issue child, 자식); one", nil},
		{"glossed list json", `holds live bindings at ["child","parent"] (issue child, 자식); one`, nil},
		{"glossed one-element list", "['supervisor'] (부모)", nil},
		{"glossed list of all three", "['child', 'parent', 'supervisor'] (issue child, 자식, 부모)", nil},
		{"first-parent", "git log --first-parent", nil},
		{"two-parent", "a two-parent merge", nil},
		{"single-parent", "a single-parent commit", nil},
		{"parent-class", "a parent-class decision", nil},
		{"parent-task flag", "--parent-task 01x", nil},
		{"identifier", "parent_task_id", nil},
		{"field path", "parent.settings", nil},
		{"inside words", "apparent supervisory parental parenthood", nil},
		{"after a digit", "2parent", nil},
		{"before a digit", "parent2", nil},
		{"first parent", "the first parent of the merge", nil},
		{"second parent", "its Second parent", nil},
		{"parent revision", "the parent revision", nil},
		{"parent revisions", "both parent revisions", nil},
		{"parent commit", "the parent commit's plugin manifest", nil},
		{"parent commits", "two parent commits", nil},
		{"parent format", "parent %d of %s", nil},
		{"has count parents", "commit %s has %d parent(s)", nil},
		{"has count parents plural", "commit %s has %d parents", nil},
		{"first parent possessive", "the first parent's tree", nil},
		{"empty", "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Find(c.text)
			if len(got) == 0 && len(c.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("Find(%q) = %+v, want %+v", c.text, got, c.want)
			}
		})
	}
}

// TestFindSkipsCommandWords is D3 of plan r3 (CRW-1477): a role word that is a crw command name 1.0
// keeps is no layer, so Find does not count it. Two forms are exempt and nothing wider: the command word
// right after `crw <family> ` (crw manage supervisor), and the skill command parent-title next to
// parent-class. Running text around a command, or another word built on parent-, still counts.
func TestFindSkipsCommandWords(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []Hit
	}{
		// Not hits: the command names.
		{"command line", "crw manage supervisor register", nil},
		{"usage line", "usage: crw manage supervisor {register,show}", nil},
		{"error prefix", "crw manage supervisor: error: x", nil},
		{"command in a sentence", "run `crw manage supervisor show` again", nil},
		{"choice list", "{hook-probe,parent-title,start-policy}", nil},
		{"choose from", "choose from issue-ready, parent-title", nil},
		{"skill command", "crw skill parent-title replay", nil},

		// Still hits.
		{"running text", "the supervisor said", []Hit{{"supervisor", 4}}},
		{"article after the family", "crw manage the supervisor", []Hit{{"supervisor", 15}}},
		{"no family", "crw supervisor", []Hit{{"supervisor", 4}}},
		{"unknown family", "crw the supervisor", []Hit{{"supervisor", 8}}},
		{"crw inside a word", "xcrw manage supervisor", []Hit{{"supervisor", 12}}},
		{"crw-dev binary", "crw-dev manage supervisor", []Hit{{"supervisor", 15}}},
		{"upper-case command word", "crw manage Supervisor", []Hit{{"Supervisor", 11}}},
		{"plural command word", "crw manage supervisors", []Hit{{"supervisors", 11}}},
		{"command word possessive", "crw manage supervisor's seat", []Hit{{"supervisor's", 11}}},
		{"second word after the command", "crw manage supervisor supervisor", []Hit{{"supervisor", 22}}},
		{"parent-titled", "a parent-titled note", []Hit{{"parent", 2}}},
		{"parent-made", "parent-made", []Hit{{"parent", 0}}},
		{"section after the command", "crw manage supervisor: error: the supervisor section", []Hit{{"supervisor", 34}}},
		{"the supervisor section", "the supervisor section", []Hit{{"supervisor", 4}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Find(c.text)
			if len(got) == 0 && len(c.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("Find(%q) = %+v, want %+v", c.text, got, c.want)
			}
		})
	}
}
