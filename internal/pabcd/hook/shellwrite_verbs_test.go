package hook

import (
	"encoding/json"
	"os"
	"slices"
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
			got := shellWriteDestsTest(c.command)
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
		if got := shellWriteDestsTest(command); len(got) != 0 {
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
	if len(golden.Entry) != 342 || len(golden.Units) != 353 {
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
			got := shellWriteDestsTest(c.Input)
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
