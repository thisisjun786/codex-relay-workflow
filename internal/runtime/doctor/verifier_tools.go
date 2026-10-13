package doctor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
)

// verifierProbeTimeout bounds each probe of a verifier tool.
const verifierProbeTimeout = 10 * time.Second

var goVersionWord = regexp.MustCompile(`\bgo version (go[0-9][^ \n]*)`)

// VerifierToolsReport is the verifierTools entry of the doctor report (CRW-1319): the verifier's tools as a run with env reads them
// (the configuration's verifier section, else PATH), the go it runs, where that go came from and its version, and whether crw-dev
// is runnable. Everything wrong is a warning; the entry never makes the report a fault.
func VerifierToolsReport(ctx context.Context, env scope.Env) Object {
	warnings := []any{}
	tools, err := crwconfig.LoadVerifierTools(env.Get, "")
	if err != nil {
		warnings = append(warnings, "the verifier section could not be read: "+err.Error())
	}
	pathEnv := env.Get("PATH")
	goBin := tools.GoBinary(pathEnv)
	var goPath, goSource, goVersion any
	if goBin != "" {
		goPath, goSource = goBin, "path"
		if tools.GoRoot != "" {
			goSource = "config"
		}
		// GOTOOLCHAIN local: the probe reads this go's own version and never selects or fetches another
		if out, err := verifierProbe(ctx, env, []string{"GOTOOLCHAIN=local"}, goBin, "version"); err != nil {
			warnings = append(warnings, "go "+goBin+" does not run: "+err.Error())
		} else if m := goVersionWord.FindStringSubmatch(string(out)); m != nil {
			goVersion = m[1]
		} else {
			warnings = append(warnings, "go "+goBin+" printed no go version")
		}
	} else {
		warnings = append(warnings, "no go: verifier.go_root is not set and go is not on PATH; the verifier cannot build or run crw-dev ci local (crw config verifier --go-root <dir>)")
	}
	if tools.GoRoot == "" && tools.CrwDev == "" && err == nil {
		warnings = append(warnings, "verifier.go_root and verifier.crw_dev are not set: the verifier takes crw-dev and go from PATH (fail-open)")
	}
	mode := tools.Mode()
	runnable := false
	var command any
	switch mode {
	case "tree":
		// the tree is the one verified; what the doctor can tell is that its go runs
		bin := "go"
		if goBin != "" {
			bin = goBin
		}
		command = []any{bin, "run", "-tags", "dev", "./cmd/crw-dev"}
		runnable = goVersion != nil
	default:
		bin := tools.CrwDev
		if mode == "path" {
			bin = lookExecutable("crw-dev", pathEnv)
		}
		if bin != "" {
			command = []any{bin}
			var extra []string
			if c, cerr := tools.Command("", pathEnv); cerr == nil {
				extra = c.Env
			}
			if _, perr := verifierProbe(ctx, env, extra, bin, "help"); perr == nil {
				runnable = true
			} else {
				warnings = append(warnings, "crw-dev "+bin+" does not run: "+perr.Error())
			}
		} else {
			warnings = append(warnings, "crw-dev is not on PATH")
		}
	}
	var config any
	if tools.Config != "" {
		config = tools.Config
	}
	return Object{
		{Key: "config", Value: config},
		{Key: "goRoot", Value: nullable(tools.GoRoot)},
		{Key: "go", Value: goPath},
		{Key: "goSource", Value: goSource},
		{Key: "goVersion", Value: goVersion},
		{Key: "crwDev", Value: Object{{Key: "mode", Value: mode}, {Key: "command", Value: command}, {Key: "runnable", Value: runnable}}},
		{Key: "warnings", Value: warnings},
	}
}

// verifierProbe runs one probe of a tool with env and the extra entries over it, bounded in time, and answers its output. The
// probe has a home of its own, removed afterwards, so the doctor writes nothing of the host's: go writes its telemetry counters
// below the user configuration directory whenever it starts.
func verifierProbe(ctx context.Context, env scope.Env, extra []string, name string, args ...string) ([]byte, error) {
	home, err := os.MkdirTemp("", "crw-doctor-probe-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(home)
	ctx, cancel := context.WithTimeout(ctx, verifierProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(append(append([]string{}, env...), extra...), "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, "config"),
		"XDG_CACHE_HOME="+filepath.Join(home, "cache"), "XDG_STATE_HOME="+filepath.Join(home, "state"), "XDG_DATA_HOME="+filepath.Join(home, "data"))
	cmd.Dir = home
	return cmd.CombinedOutput()
}

// lookExecutable is the executable named name in a directory of pathEnv, or "".
func lookExecutable(name, pathEnv string) string {
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		if path, err := exec.LookPath(candidate); err == nil {
			return path
		}
	}
	return ""
}
