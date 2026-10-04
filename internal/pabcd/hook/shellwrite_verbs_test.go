package hook

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

// The B cases of shell-write-destinations.test.ts:40-55 and :91-100 (verbs, python and node writes) and the POSIX part of
// :117-134. PowerShell and .NET cases are not ported: the oracle's own answers for them are outside this port's scope.
func TestShellVerbB(t *testing.T) {
	const mem = "/h/memories"
	for _, c := range []struct {
		command string
		want    []string
	}{
		{"rg foo /w | tee " + mem + "/out.md", []string{mem + "/out.md"}},
		{"tee -a " + mem + "/out.md", []string{mem + "/out.md"}},
		{"sed -n '1p' " + mem + "/M.md", []string{}},
		{"sed -i 's/a/b/' " + mem + "/M.md", []string{mem + "/M.md"}},
		{"sed -i '' 's/a/b/' " + mem + "/M.md", []string{mem + "/M.md"}},
		{"sed -i.bak -e 's/a/b/' " + mem + "/M.md", []string{mem + "/M.md"}},
		{"cp /w/a.md " + mem + "/b.md", []string{mem + "/b.md"}},
		{"cp " + mem + "/a.md /w/b.md", []string{"/w/b.md"}},
		{"mv /w/a.md " + mem + "/b.md", []string{mem + "/b.md"}},
		{"cp -t " + mem + " /w/a.md", []string{mem}},
		{"perl -i -pe 's/a/b/' " + mem + "/M.md", []string{mem + "/M.md"}},
		{"ruby -i -pe 's/a/b/' " + mem + "/M.md", []string{mem + "/M.md"}},
		{"sudo tee " + mem + "/out.md", []string{mem + "/out.md"}},
		{"cat " + mem + "/M.md", []string{}},
		{"python -c \"open(r'" + mem + "/n.md','w').write('x')\"", []string{mem + "/n.md"}},
		{"python3 -c \"from pathlib import Path; Path('" + mem + "/n.md').write_text('x')\"", []string{mem + "/n.md"}},
		{"node -e \"require('fs').writeFileSync('" + mem + "/n.md','x')\"", []string{mem + "/n.md"}},
		{"python.exe -c \"open('" + mem + "/n.md','w').write('x')\"", []string{mem + "/n.md"}},
		{"python3 -c \"from pathlib import Path; print(Path('" + mem + "/MEMORY.md').read_text()); print('x -> y')\"", []string{}},
		{"py -c \"open(r'" + mem + "/n.md','w').write('x')\"", []string{mem + "/n.md"}},
		{"node --eval \"require('fs').writeFileSync('" + mem + "/n.md','x')\"", []string{mem + "/n.md"}},
		{"node -erequire('fs').writeFileSync('" + mem + "/n.md','x')", []string{mem + "/n.md"}},
		{"sc query", []string{}},
		{"cat " + mem + "/n.md", []string{}},
		{"gc " + mem + "/n.md", []string{}},
	} {
		t.Run(c.command, func(t *testing.T) {
			got := ShellWriteDestinations(c.command)
			if got == nil || !slices.Equal(got, c.want) {
				t.Fatalf("got %q, want non-nil %q", got, c.want)
			}
		})
	}
}

// The oracle's PowerShell and .NET detection is not ported (POSIX only): these name no destination.
func TestShellVerbPowerShellNotPorted(t *testing.T) {
	for _, command := range []string{
		"Set-Content -LiteralPath '/m/n.md' -Value x", "Out-File -FilePath /m/n.md", "New-Item -Path /m/n.md -ItemType File",
		"Copy-Item /w/a.md /m/b.md", "Tee-Object -FilePath /m/out.md", "copy /w/a.md /m/b.md", "sc /m/n.md", "ni /m/n.md",
		"[IO.File]::WriteAllText('/m/n.md','x')", "[System.IO.File]::AppendAllText('/m/n.md','x')",
	} {
		if got := ShellWriteDestinations(command); len(got) != 0 {
			t.Fatalf("%q named %q", command, got)
		}
	}
}

type shellVerbGolden struct {
	Entry []struct {
		Input            string
		Output, Expected []string
		Classification   string
		Reason           string
	}
	Units []struct {
		Fn, Str, Verb string
		List          []string
		Output        json.RawMessage
	}
}

func shellVerbLoadGolden(t *testing.T) shellVerbGolden {
	t.Helper()
	var golden shellVerbGolden
	raw, err := os.ReadFile("testdata/shellwrite/verbs-oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	if len(golden.Entry) != 341 || len(golden.Units) != 353 {
		t.Fatalf("incomplete recording: %d entries, %d units", len(golden.Entry), len(golden.Units))
	}
	return golden
}

// Every recorded command is answered with the oracle's reports first; a command the oracle already named correctly is
// answered exactly, and each other answer is the oracle's followed by the destinations this port adds (security).
func TestShellVerbRecordedEntries(t *testing.T) {
	changed := 0
	for i, c := range shellVerbLoadGolden(t).Entry {
		if c.Classification != "identical" && (c.Classification != "intentionally-changed" || c.Reason == "") {
			t.Fatalf("entry %d is not classified", i)
		}
		if c.Classification == "intentionally-changed" {
			changed++
		}
		t.Run(c.Input, func(t *testing.T) {
			got := ShellWriteDestinations(c.Input)
			if !slices.Equal(got, c.Expected) {
				t.Fatalf("got %q want %q", got, c.Expected)
			}
			if len(got) < len(c.Output) || !slices.Equal(got[:len(c.Output)], c.Output) {
				t.Fatalf("lost oracle reports %q in %q", c.Output, got)
			}
		})
	}
	if changed == 0 {
		t.Fatal("no intentionally changed case recorded")
	}
}

// The recorded answers of each oracle function, replayed against its port (the additions of the hardened reading are not part
// of these functions).
func TestShellVerbRecordedUnits(t *testing.T) {
	for i, c := range shellVerbLoadGolden(t).Units {
		t.Run(fmt.Sprintf("%s/%d", c.Fn, i), func(t *testing.T) {
			var got any
			switch c.Fn {
			case "basename":
				got = shellVerbBasename(c.Str)
			case "normalizeVerb":
				got = shellVerbNormalize(c.Str)
			case "stripPrefixes":
				got = shellVerbStripPrefixes(c.List)
			case "teeDestinations":
				got = shellVerbTee(c.List)
			case "sedInPlaceDestinations":
				got = shellVerbSed(c.List)
			case "cpMvDestinations":
				got = shellVerbCpMv(c.List)
			case "interpInPlaceDestinations":
				got = shellVerbInterp(c.List, false)
			case "pythonNodeWriteDestinations":
				got = shellVerbPythonNode(c.Verb, c.List, false)
			case "scriptWriteDestinations":
				got = shellVerbScriptWrites(c.Str, false)
			default:
				t.Fatalf("unknown function %s", c.Fn)
			}
			a, _ := json.Marshal(got)
			if string(a) == "null" {
				a = []byte("[]")
			}
			var expected any
			if err := json.Unmarshal(c.Output, &expected); err != nil {
				t.Fatal(err)
			}
			if e, _ := json.Marshal(expected); string(a) != string(e) {
				t.Fatalf("%q %q %q: got %s want %s", c.Str, c.Verb, c.List, a, e)
			}
		})
	}
}

// A command string quoted into a shell -c, level after level, is read; the work is bounded by a budget of bytes, past which only the
// oracle's reading stays.
func TestShellVerbNestedShells(t *testing.T) {
	command := "tee /m/a"
	for range 6 {
		command = "bash -c \"" + strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(command) + "\""
	}
	if got := ShellWriteDestinations(command); !slices.Equal(got, []string{"/m/a"}) {
		t.Fatalf("six levels: %q", got)
	}
	spent := 0
	if got := shellVerbNested("bash -c 'tee /m/a'", &spent); len(got) != 0 {
		t.Fatalf("budget spent: %q", got)
	}
	ample := 1 << 20
	if got := shellVerbNested("bash -c 'tee /m/a'", &ample); !slices.Equal(got, []string{"/m/a"}) {
		t.Fatalf("budget left: %q", got)
	}
	if got := ShellWriteDestinations("eval builtin " + strings.Repeat("eval builtin ", 3000) + "tee /m/a"); !slices.Equal(got, []string{"/m/a"}) {
		t.Fatalf("eval chain: %q", got)
	}
}
