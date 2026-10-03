package contracttest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/cxccorpus"
)

// cxcRecDir names the records directory of a replay case. A process that has it set is one of the
// case's stub programs or its git wrapper, symlinks to this test binary, and TestMain hands it to
// cxcHelper before anything else. It does what the recorder's Node stubs do: append {cmd, argv,
// cwd} to the call log, then answer as the scenario scripted (the first case whose argv is a prefix
// of the call, else the stub's own answer, else 127) or, for git, run the real one.
const cxcRecDir = "CXC_REC_DIR"

func cxcHelper(dir string) int {
	name, argv := filepath.Base(os.Args[0]), append([]string{}, os.Args[1:]...)
	cwd, _ := syscall.Getwd()
	line, _ := json.Marshal(cxccorpus.Call{Cmd: name, Argv: argv, Cwd: cwd})
	if log, err := os.OpenFile(os.Getenv("CXC_REC_LOG"), os.O_APPEND|os.O_WRONLY, 0); err == nil {
		_, _ = log.Write(append(line, '\n'))
		_ = log.Close()
	}
	if name == "git" {
		err := syscall.Exec(os.Getenv("CXC_REC_GIT"), append([]string{"git"}, argv...), os.Environ())
		fmt.Fprintln(os.Stderr, "git:", err)
		return 127
	}
	var answer cxccorpus.Stub
	if raw, err := os.ReadFile(filepath.Join(dir, "stubs", name+".json")); err != nil || json.Unmarshal(raw, &answer) != nil {
		fmt.Fprintf(os.Stderr, "stub %s: not scripted\n", name)
		return 127
	}
	for _, c := range answer.Cases {
		if len(argv) >= len(c.Argv) && slices.Equal(argv[:len(c.Argv)], c.Argv) {
			answer = cxccorpus.Stub{Stdout: c.Stdout, Stderr: c.Stderr, Exit: c.Exit}
			break
		}
	}
	fmt.Fprint(os.Stdout, answer.Stdout)
	fmt.Fprint(os.Stderr, answer.Stderr)
	return answer.Exit
}
