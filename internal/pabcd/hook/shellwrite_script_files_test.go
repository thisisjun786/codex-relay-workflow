package hook

import (
	"reflect"
	"sort"
	"testing"
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
		// A write to /dev/null changes no file, so the gate drops it, as it does for tee /dev/null.
		{"control: /dev/null operand", "script -qec true /dev/null", nil},
		{"log files with -c and no file operand", "script -I in.log -O out.log -B io.log -T tm.log -c STRING", []string{"in.log", "out.log", "io.log", "tm.log", "typescript"}},
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
