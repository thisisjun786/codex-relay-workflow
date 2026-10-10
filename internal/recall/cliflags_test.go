package recall

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestRecallFlagsOracle(t *testing.T) {
	seen := map[string]int{}
	for i, c := range formatOracleRows(t) {
		var got any
		var err error
		switch c.Fn {
		case "help":
			args := arg[[]string](t, c.oracleCase, 0)
			got = WantsHelp(args)
			// port: fixed (CRW-1125, known-defects.md :667): the oracle scans every argument, the port stops at the -- terminator.
			if end := slices.Index(args, "--"); end >= 0 {
				c.Out = json.RawMessage(strconv.FormatBool(slices.Contains(args[:end], "--help") || slices.Contains(args[:end], "-h")))
			}
		case "parse":
			got, err = ParseFlags(arg[[]string](t, c.oracleCase, 0))
		case "read":
			parsed, e := ReadFlags(arg[[]string](t, c.oracleCase, 0))
			if e != nil {
				if e.Error()+"\n" != c.Stderr {
					t.Errorf("case %d stderr %q, oracle %q", i, e.Error()+"\n", c.Stderr)
				}
				got = nil
			} else {
				if c.Stderr != "" {
					t.Errorf("case %d unexpected success", i)
				}
				got = parsed
			}
		case "path":
			err = FlagLikePathError(arg[map[string]any](t, c.oracleCase, 0))
			if err != nil {
				got = err.Error()
				err = nil
			}
		case "num":
			got = NumFlag(arg[map[string]any](t, c.oracleCase, 0), arg[string](t, c.oracleCase, 1))
		case "usage":
			got = Usage()
		default:
			continue
		}
		seen[c.Fn]++
		if c.Error != "" {
			if err == nil || err.Error() != c.Error {
				t.Errorf("case %d %s error %v; oracle %s", i, c.Fn, err, c.Error)
			}
			continue
		}
		if err != nil {
			t.Errorf("case %d %s: %v", i, c.Fn, err)
			continue
		}
		var want any
		if e := json.Unmarshal(c.Out, &want); e != nil {
			t.Fatal(e)
		}
		if actual := canon(t, got); !reflect.DeepEqual(actual, want) {
			t.Errorf("case %d %s: got %#v; oracle %#v", i, c.Fn, actual, want)
		}
	}
	if len(seen) != 6 {
		t.Fatalf("flag oracle functions = %d", len(seen))
	}
	t.Logf("replayed flag oracle groups: %v", seen)
}

func TestRecallReadFlagsAndHelpPrecedence(t *testing.T) {
	for _, args := range [][]string{{"--unknown", "--help"}, {"--cwd", "-x", "-h"}, {"--help", "--", "x"}} {
		if !WantsHelp(args) {
			t.Errorf("help must precede parser: %v", args)
		}
	}
	for _, args := range [][]string{{"--", "--help"}, {"x", "--", "-h"}} {
		if WantsHelp(args) {
			t.Errorf("a flag after the terminator is a query word (CRW-1125, :667): %v", args)
		}
	}
	for _, query := range []string{"help", "/?", "--help=value"} {
		if WantsHelp([]string{query}) {
			t.Errorf("not a help flag: %s", query)
		}
	}
	for _, key := range []string{"cwd", "cwd-only", "home", "index-path"} {
		if _, err := ReadFlags([]string{"--" + key + "=-x"}); err == nil || !strings.Contains(err.Error(), "path must not start") {
			t.Errorf("path %s = %v", key, err)
		}
	}
	parsed, err := ReadFlags([]string{"help", "--cwd", "."})
	if err != nil || !reflect.DeepEqual(parsed.Positionals, []string{"help"}) {
		t.Fatal(parsed, err)
	}
	if _, err = ReadFlags([]string{"--home", "-x"}); err == nil || !strings.HasPrefix(err.Error(), "Option '--home' argument is ambiguous.") {
		t.Fatal("parse error must precede path check", err)
	}
}

func TestRecallExplicitHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home)
	t.Setenv("CRW_HOME", home)
	p, err := ExplicitHome(map[string]any{})
	if p != nil || err != nil {
		t.Fatal(p, err)
	}
	p, err = ExplicitHome(map[string]any{"home": true})
	if p != nil || err != nil {
		t.Fatal(p, err)
	}
	p, err = ExplicitHome(map[string]any{"home": home})
	if p == nil || *p != home || err != nil {
		t.Fatal(p, err)
	}
	missing := filepath.Join(home, "missing")
	if _, err = ExplicitHome(map[string]any{"home": missing}); err == nil || err.Error() != "--home not found: "+missing {
		t.Fatal(err)
	}
	file := filepath.Join(home, "file")
	if err = os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = ExplicitHome(map[string]any{"home": file}); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatal("an existing non-directory is no home (CRW-1125, :668)", err)
	}
	if err = os.Symlink(missing, filepath.Join(home, "dangling")); err != nil {
		t.Fatal(err)
	}
	if _, err = ExplicitHome(map[string]any{"home": filepath.Join(home, "dangling")}); err == nil {
		t.Fatal("dangling symlink counted as present")
	}
}
