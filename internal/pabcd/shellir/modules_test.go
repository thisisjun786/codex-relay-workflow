package shellir

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestCRW1085ModuleBoundsAndSpecialFiles(t *testing.T) {
	for _, kind := range []string{"files", "bytes", "fifo", "shadow", "written", "unknown"} {
		t.Run(kind, func(t *testing.T) {
			cwd := t.TempDir()
			cmd := "python3 -m unittest"
			switch kind {
			case "files":
				for i := range maxModuleFiles + 1 {
					if err := os.WriteFile(filepath.Join(cwd, fmt.Sprintf("test%d.py", i)), []byte("print(1)"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "bytes":
				if err := os.WriteFile(filepath.Join(cwd, "test_big.py"), []byte(strings.Repeat("#", maxModuleBytes+1)), 0600); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := syscall.Mkfifo(filepath.Join(cwd, "test_pipe.py"), 0600); err != nil {
					t.Fatal(err)
				}
			case "shadow":
				if err := os.WriteFile(filepath.Join(cwd, "unittest.py"), []byte("print(1)"), 0600); err != nil {
					t.Fatal(err)
				}
			case "written":
				cmd = "printf x > test_new.py; python3 -m unittest test_new.py"
			case "unknown":
				cmd = `cd "$DIR"; python3 -m unittest`
			}
			if _, err := Analyze(cmd, cwd); err == nil {
				t.Fatal("unproved module allowed")
			}
		})
	}
}

func TestCRW1085NoExecDoesNotHideExecution(t *testing.T) {
	for _, cmd := range []string{`bash -n +n -c 'echo x'`, `bash -n +o noexec -c 'echo x'`, `ruby -v -e 'puts 1'`, `node -v -e 'console.log(1)'`} {
		res, err := Analyze(cmd, t.TempDir())
		if err != nil {
			continue
		}
		found := false
		for _, e := range res.Execs {
			found = found || e.Name == "echo" || e.Inline != nil
		}
		if !found {
			t.Errorf("execution hidden: %s", cmd)
		}
	}
}
