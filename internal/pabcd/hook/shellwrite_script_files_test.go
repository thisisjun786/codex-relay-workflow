package hook

import (
	"reflect"
	"sort"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/shellir"
)

// TestShellIRScriptFileDests pins the files util-linux script writes: its transcript (the file operand, else typescript in
// the directory the program runs in) and its -I, -O, -B and -T log files. The -c value is shell code, never a file.
func TestShellIRScriptFileDests(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want []string
	}{
		{"default transcript, no file operand", "script -qec /bin/true", []string{"typescript"}},
		{"relative -c string leaves no phantom", "script -qec 'echo hi' ", []string{"typescript"}},
		{"absolute -c string leaves no phantom", "script -c /bin/true", []string{"typescript"}},
		{"file operand is the transcript", "script -qec true out.log", []string{"out.log"}},
		{"append mode keeps the file operand", "script -a -c 'echo hi' log", []string{"log"}},
		// The correction message asked this control to name /dev/null. The record does name it (see
		// TestShellIRScriptDevNullIsDroppedByTheUniqueStep); shellIRUnique then drops every /dev/null destination, as it does for
		// tee /dev/null and 2>/dev/null, because a write to /dev/null changes no file and no protected path is /dev/null. The drop
		// is kept by decision (docs/port-cxc/known-defects/CRW-1028.md), so the final answer is empty.
		{"control: /dev/null operand is named by the record and dropped as a destination", "script -qec true /dev/null", nil},
		// util-linux 2.41.3 (the host): an output log named by -I, -O or -B replaces the default transcript; -T (timing) and -a do not.
		{"log files with -c and no file operand name the four files", "script -I in.log -O out.log -B io.log -T tm.log -c STRING", []string{"in.log", "out.log", "io.log", "tm.log"}},
		{"an output log alone replaces typescript", "script -O out.log -c true", []string{"out.log"}},
		{"-B alone replaces typescript", "script -qe -B io.log -c true", []string{"io.log"}},
		{"timing alone keeps typescript", "script -T tm.log -c true", []string{"tm.log", "typescript"}},
		{"append mode alone keeps typescript", "script -a -c true", []string{"typescript"}},
		{"log files with a file operand", "script -O out.log -c true tr.log", []string{"out.log", "tr.log"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := shellIRWriteDests(c.cmd, "/work", nil)
			if !ok {
				t.Fatalf("%q: unreadable", c.cmd)
			}
			sort.Strings(got)
			want := append([]string{}, c.want...)
			sort.Strings(want)
			if (len(got) != 0 || len(want) != 0) && !reflect.DeepEqual(got, want) {
				t.Errorf("%q: dests %q, want %q", c.cmd, got, want)
			}
		})
	}
}

// TestShellIRScriptTranscriptFollowsCd: the default transcript is written in the directory the script runs in, so a
// cd before it moves the destination the memory gate sees.
func TestShellIRScriptTranscriptFollowsCd(t *testing.T) {
	got, ok := shellIRWriteDestsResolved("cd /memdir && script -qec /bin/true", "/work", nil)
	if !ok {
		t.Fatal("unreadable")
	}
	if want := []string{"/memdir/typescript"}; !reflect.DeepEqual(got, want) {
		t.Errorf("dests %q, want %q", got, want)
	}
}

// TestShellIRScriptDevNullIsDroppedByTheUniqueStep: the wrapper-file record of script -qec true /dev/null names /dev/null (the
// control of the correction message), and the step that makes the destinations unique drops it. Both facts are pinned, so the drop
// cannot be lost by a change to the record and the record cannot lose the name behind the drop.
func TestShellIRScriptDevNullIsDroppedByTheUniqueStep(t *testing.T) {
	res, err := shellir.AnalyzeEnv("script -qec true /dev/null", "/work", nil)
	if err != nil {
		t.Fatal(err)
	}
	var named []string
	for _, e := range res.Execs {
		if e.Name == shellir.FileRecordName {
			named = append(named, shellIRVerbDests(e)...)
		}
	}
	if !reflect.DeepEqual(named, []string{"/dev/null"}) {
		t.Fatalf("the wrapper-file record names %q, want [/dev/null]", named)
	}
	if got := shellIRUnique(named); len(got) != 0 {
		t.Errorf("shellIRUnique kept %q, want it to drop /dev/null", got)
	}
	if got := shellIRUnique([]string{"/dev/null", "typescript", "/dev/null"}); !reflect.DeepEqual(got, []string{"typescript"}) {
		t.Errorf("shellIRUnique = %q, want [typescript]", got)
	}
}
