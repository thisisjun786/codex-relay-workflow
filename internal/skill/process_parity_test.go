package skill

import (
	"os"
	"strings"
)

// oracleEnv is the environment the Go commands whose answers are goldens run in, the one the
// Python oracles ran in when those answers were first taken (decision 29b): Python picked its
// stdin error handler from the locale, so PYTHONIOENCODING=utf-8:strict pinned the host
// behaviour and LC_ALL=C.UTF-8 the UTF-8 file encoding. The Go commands read bytes and never
// consult the locale.
func oracleEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if key == "LANG" || key == "LANGUAGE" || strings.HasPrefix(key, "LC_") ||
			key == "PYTHONUTF8" || key == "PYTHONIOENCODING" || key == "PYTHONCOERCECLOCALE" {
			continue
		}
		env = append(env, kv)
	}
	return append(append(env, "LC_ALL=C.UTF-8", "PYTHONIOENCODING=utf-8:strict"), extra...)
}

// normalizedAnswer is an answer as the goldens hold it: each UNREACHED line reduced to its
// function (unreachedByFunction).
func normalizedAnswer(answer skillProcessResult) skillProcessResult {
	answer.stdout = unreachedByFunction(answer.stdout)
	return answer
}

// unreachedByFunction reduces each line hook-probe replay prints for an unreached return to
// `  UNREACHED <function>`. The reference named a return by its Python line and source, the Go
// replay names it by its ordinal and label (hookReturnSites), so the goldens first taken from the
// reference hold which functions keep unreached returns, in order and with multiplicity;
// TestHookReplayNamesEachUnreachedReturn pins the Go lines themselves.
func unreachedByFunction(out string) string {
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if rest, ok := strings.CutPrefix(line, "  UNREACHED "); ok {
			if end := strings.IndexAny(rest, ":#"); end >= 0 {
				lines[i] = "  UNREACHED " + rest[:end]
			}
		}
	}
	return strings.Join(lines, "\n")
}
