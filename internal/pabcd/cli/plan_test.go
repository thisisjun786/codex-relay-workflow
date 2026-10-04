package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// These expectations were recorded by Node v24 from CXC v0.2.40, not from Go.
func planOracle(t *testing.T) struct {
	Helpers []struct {
		Input, Slug string
		Split       struct {
			Date *string
			Rest string
		}
	}
	Parse []struct {
		Argv   []string
		Result json.RawMessage
	}
	Help, Creation PlanCliResult
	Docs           map[string]string
} {
	t.Helper()
	var out struct {
		Helpers []struct {
			Input, Slug string
			Split       struct {
				Date *string
				Rest string
			}
		}
		Parse []struct {
			Argv   []string
			Result json.RawMessage
		}
		Help, Creation PlanCliResult
		Docs           map[string]string
	}
	raw, err := os.ReadFile("testdata/plan/oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPlanOracleHelpersAndParsing(t *testing.T) {
	o := planOracle(t)
	for i, c := range o.Helpers {
		t.Run(fmt.Sprintf("helper-%d", i), func(t *testing.T) {
			date, rest := SplitDatePrefix(c.Input)
			if !reflect.DeepEqual(date, c.Split.Date) || rest != c.Split.Rest {
				t.Errorf("split = %v,%q; want %v,%q", date, rest, c.Split.Date, c.Split.Rest)
			}
			if got := DerivePlanSlug(c.Input); got != c.Slug {
				t.Errorf("slug = %q; want %q", got, c.Slug)
			}
		})
	}
	for i, c := range o.Parse {
		t.Run(fmt.Sprintf("parse-%d", i), func(t *testing.T) {
			args, err := ParsePlanCliArgs(c.Argv, "WORKSPACE")
			var expected struct{ Error string }
			if e := json.Unmarshal(c.Result, &expected); e != nil {
				t.Fatal(e)
			}
			if expected.Error != "" {
				if err == nil || err.Error() != expected.Error {
					t.Fatalf("error = %v; want %q", err, expected.Error)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var want PlanCliArgs
			if err := json.Unmarshal(c.Result, &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(args, want) {
				t.Fatalf("args = %#v; want %#v", args, want)
			}
		})
	}
}

func parsedPlan(t *testing.T, cwd string, argv ...string) PlanCliArgs {
	t.Helper()
	args, err := ParsePlanCliArgs(argv, cwd)
	if err != nil {
		t.Fatal(err)
	}
	return args
}

func successfulPlan(t *testing.T, args PlanCliArgs) PlanCliResult {
	t.Helper()
	r := RunPlanCli(args)
	if r.Code != 0 {
		t.Fatalf("run: %#v", r)
	}
	return r
}

func planNames(t *testing.T, cwd string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(cwd, "devlog", "_plan"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// The eleven B-class plan-cli.test.ts cases, exercised in temporary workspaces.
func TestPlanParseRequirements(t *testing.T) {
	cwd := t.TempDir()
	if a := parsedPlan(t, cwd); a.Verb != "help" {
		t.Fatal(a)
	}
	for _, c := range []struct {
		args  []string
		error string
	}{
		{[]string{"nope"}, "unknown plan verb"}, {[]string{"init"}, "requires a <slug>"}, {[]string{"init", "x", "--phases", "0"}, "1-9"},
	} {
		if _, err := ParsePlanCliArgs(c.args, cwd); err == nil || !strings.Contains(err.Error(), c.error) {
			t.Fatalf("%v: %v", c.args, err)
		}
	}
	a := parsedPlan(t, cwd, "init", "My Big Feature!", "--phases", "3")
	if a.Slug != "my-big-feature" || a.Phases != 3 {
		t.Fatal(a)
	}
}

func TestPlanScaffoldsAndRefusesOverwrite(t *testing.T) {
	cwd := t.TempDir()
	args := parsedPlan(t, cwd, "init", "260821_oracle_unit", "--phases", "9")
	o := planOracle(t)
	r := successfulPlan(t, args)
	if r != o.Creation {
		t.Fatalf("result = %#v; want %#v", r, o.Creation)
	}
	unit := filepath.Join(cwd, "devlog", "_plan", "260821_oracle_unit")
	entries, err := os.ReadDir(unit)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(o.Docs) {
		t.Fatalf("files = %d", len(entries))
	}
	for name, want := range o.Docs {
		got, err := os.ReadFile(filepath.Join(unit, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("%s differs from Node document", name)
		}
	}
	for n := 1; n <= 9; n++ {
		if got := phaseDoc(n, args.Slug); got != o.Docs[fmt.Sprintf("%03d_phase%d.md", n*10, n)] {
			t.Errorf("phaseDoc %d differs", n)
		}
	}
	if planDoc(args.Slug) != o.Docs["000_plan.md"] {
		t.Error("planDoc differs")
	}
	path := filepath.Join(unit, "000_plan.md")
	if err := os.WriteFile(path, []byte("USER EDIT"), 0o644); err != nil {
		t.Fatal(err)
	}
	again := RunPlanCli(args)
	want := fmt.Sprintf("plan init: %s already exists — refusing to overwrite. Write your docs there.", unit)
	if again.Code != 1 || again.Output != want {
		t.Fatal(again)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "USER EDIT" {
		t.Fatalf("existing document lost: %q %v", b, err)
	}
}

func TestPlanSplitDateSeparators(t *testing.T) {
	for _, s := range []string{"260821_win-linux", "260821-win-linux"} {
		d, r := SplitDatePrefix(s)
		if d == nil || *d != "260821" || r != "win-linux" {
			t.Fatalf("%q: %v %q", s, d, r)
		}
	}
	if d, r := SplitDatePrefix("win-linux"); d != nil || r != "win-linux" {
		t.Fatal(d, r)
	}
}
func TestPlanNonDateNumericPrefix(t *testing.T) {
	for _, s := range []string{"12345_thing", "1234567_thing"} {
		if d, r := SplitDatePrefix(s); d != nil || r != s {
			t.Fatal(d, r)
		}
	}
}
func TestPlanSlugUnderscores(t *testing.T) {
	if DerivePlanSlug("my_slug") != "my_slug" || DerivePlanSlug("My Big Feature!") != "my-big-feature" {
		t.Fatal("slug normalization")
	}
}
func TestPlanPrefixedDateNotDoubled(t *testing.T) {
	cwd := t.TempDir()
	a := parsedPlan(t, cwd, "init", "260821_win-linux-optimization", "--cwd", cwd)
	if a.Date == nil || *a.Date != "260821" {
		t.Fatal(a)
	}
	successfulPlan(t, a)
	if names := planNames(t, cwd); !slices.Equal(names, []string{"260821_win-linux-optimization"}) {
		t.Fatal(names)
	}
}
func TestPlanHyphenDateConvention(t *testing.T) {
	cwd := t.TempDir()
	successfulPlan(t, parsedPlan(t, cwd, "init", "260821-win-linux"))
	if names := planNames(t, cwd); !slices.Equal(names, []string{"260821_win-linux"}) {
		t.Fatal(names)
	}
}
func TestPlanTodayLocalDate(t *testing.T) {
	cwd := t.TempDir()
	a := parsedPlan(t, cwd, "init", "fresh_unit")
	if a.Date != nil {
		t.Fatal(a)
	}
	before := YYMMDD(time.Now())
	successfulPlan(t, a)
	after := YYMMDD(time.Now())
	names := planNames(t, cwd)
	if len(names) != 1 || (names[0] != before+"_fresh_unit" && names[0] != after+"_fresh_unit") {
		t.Fatal(names)
	}
	for _, c := range []struct {
		date time.Time
		want string
	}{
		{time.Date(2026, 1, 2, 0, 0, 0, 0, time.FixedZone("local", -7*3600)), "260102"},
		{time.Date(2000, 12, 31, 0, 0, 0, 0, time.UTC), "001231"},
	} {
		if got := YYMMDD(c.date); got != c.want {
			t.Fatalf("date %v = %q; want %q", c.date, got, c.want)
		}
	}
}
func TestPlanDateOnlyRejected(t *testing.T) {
	if _, err := ParsePlanCliArgs([]string{"init", "260821_"}, t.TempDir()); err == nil || !strings.Contains(err.Error(), "no usable slug") {
		t.Fatal(err)
	}
}
func TestPlanSuccessIsRelative(t *testing.T) {
	cwd := t.TempDir()
	r := successfulPlan(t, parsedPlan(t, cwd, "init", "260821_relative-check"))
	if !strings.Contains(r.Output, "devlog") || strings.Contains(r.Output, cwd) {
		t.Fatal(r)
	}
}
func TestPlanDecades(t *testing.T) {
	cwd := t.TempDir()
	successfulPlan(t, parsedPlan(t, cwd, "init", "260821_decade-check", "--phases", "9"))
	files, err := os.ReadDir(filepath.Join(cwd, "devlog", "_plan", "260821_decade-check"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 10 || files[9].Name() != "090_phase9.md" {
		t.Fatal(files)
	}
}

func TestPlanHelpOracle(t *testing.T) {
	o := planOracle(t)
	want := o.Help
	want.Output = strings.ReplaceAll(want.Output, "cxc plan", "crw pabcd plan")
	if got := RunPlanCli(parsedPlan(t, t.TempDir(), "--help")); got != want {
		t.Fatalf("help = %#v; want %#v", got, want)
	}
}

func TestPlanDirectTenthPhaseOracle(t *testing.T) {
	var o struct {
		DirectTen struct {
			Result   PlanCliResult
			Document string
		}
	}
	raw, err := os.ReadFile("testdata/plan/oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	date := "260821"
	r := successfulPlan(t, PlanCliArgs{Verb: "init", Cwd: cwd, Date: &date, Slug: "unit", Phases: 10})
	if r != o.DirectTen.Result {
		t.Fatal(r)
	}
	doc, err := os.ReadFile(filepath.Join(cwd, "devlog", "_plan", "260821_unit", "100_phase10.md"))
	if err != nil || string(doc) != o.DirectTen.Document {
		t.Fatalf("tenth document differs: %v", err)
	}
}

func TestPlanRejectsSymlinkParents(t *testing.T) {
	for _, part := range []string{"devlog", "_plan"} {
		for _, target := range []string{"inside", "outside", "dangling"} {
			t.Run(part+"-"+target, func(t *testing.T) {
				cwd := t.TempDir()
				dest := filepath.Join(cwd, "target")
				if target == "outside" {
					dest = t.TempDir()
				}
				if target != "dangling" {
					if err := os.MkdirAll(dest, 0o755); err != nil {
						t.Fatal(err)
					}
				}
				parent := cwd
				if part == "_plan" {
					parent = filepath.Join(cwd, "devlog")
					if err := os.Mkdir(parent, 0o755); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Symlink(dest, filepath.Join(parent, part)); err != nil {
					t.Fatal(err)
				}
				r := RunPlanCli(parsedPlan(t, cwd, "init", "260821_safe"))
				if r.Code != 1 || !strings.Contains(r.Output, "symlink") {
					t.Fatal(r)
				}
				if target != "dangling" {
					if e, err := os.ReadDir(dest); err != nil || len(e) != 0 {
						t.Fatalf("target modified: %v %v", e, err)
					}
				}
			})
		}
	}
}

func TestPlanUnsafeDirectComponents(t *testing.T) {
	for _, c := range []struct{ date, slug string }{{"../../../escape", "unit"}, {"260821", "../../escape"}, {"260821", `x\escape`}} {
		cwd := t.TempDir()
		r := RunPlanCli(PlanCliArgs{Verb: "init", Date: &c.date, Slug: c.slug, Phases: 1, Cwd: cwd})
		if r.Code != 1 {
			t.Fatal(r)
		}
		if entries, err := os.ReadDir(cwd); err != nil || len(entries) != 0 {
			t.Fatal(entries, err)
		}
	}
}

func TestPlanExistingUnitKinds(t *testing.T) {
	for _, kind := range []string{"file", "live-link", "dangling-link"} {
		t.Run(kind, func(t *testing.T) {
			cwd := t.TempDir()
			base := filepath.Join(cwd, "devlog", "_plan")
			if err := os.MkdirAll(base, 0o755); err != nil {
				t.Fatal(err)
			}
			unit := filepath.Join(base, "260821_existing")
			if kind == "file" {
				if err := os.WriteFile(unit, []byte("original"), 0o644); err != nil {
					t.Fatal(err)
				}
			} else {
				target := filepath.Join(t.TempDir(), "target")
				if kind == "live-link" {
					if err := os.Mkdir(target, 0o755); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Symlink(target, unit); err != nil {
					t.Fatal(err)
				}
			}
			if r := RunPlanCli(parsedPlan(t, cwd, "init", "260821_existing")); r.Code != 1 || !strings.Contains(r.Output, "already exists") {
				t.Fatal(r)
			}
		})
	}
}

func TestPlanPostMkdirExclusiveDocument(t *testing.T) {
	for _, kind := range []string{"file", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			cwd := t.TempDir()
			victim := filepath.Join(t.TempDir(), "records.json")
			if err := os.WriteFile(victim, []byte("ORIGINAL_RECORD"), 0o644); err != nil {
				t.Fatal(err)
			}
			r := runPlanCli(parsedPlan(t, cwd, "init", "260821_race"), func(root *planDir, name, data string) error {
				if kind == "file" {
					if err := os.WriteFile(filepath.Join(root.Name(), name), []byte("ORIGINAL_RECORD"), 0o644); err != nil {
						return err
					}
				} else {
					if err := os.Symlink(victim, filepath.Join(root.Name(), name)); err != nil {
						return err
					}
				}
				return writePlanDoc(root, name, data)
			})
			if r.Code != 1 {
				t.Fatalf("exclusive document accepted existing %s: %#v", kind, r)
			}
			path := victim
			if kind == "file" {
				path = filepath.Join(cwd, "devlog", "_plan", "260821_race", "000_plan.md")
			}
			b, err := os.ReadFile(path)
			if err != nil || string(b) != "ORIGINAL_RECORD" {
				t.Fatalf("record truncated: %q %v", b, err)
			}
		})
	}
}

func TestPlanPartialUnitPreserved(t *testing.T) {
	cwd := t.TempDir()
	a := parsedPlan(t, cwd, "init", "260821_partial", "--phases", "2")
	calls := 0
	r := runPlanCli(a, func(root *planDir, name, data string) error {
		calls++
		if calls == 2 {
			return &os.PathError{Op: "write", Path: filepath.Join(root.Name(), name), Err: syscall.EIO}
		}
		return writePlanDoc(root, name, data)
	})
	if r.Code != 1 || r.Output != "plan init failed: EIO: i/o error, write" {
		t.Fatal(r)
	}
	unit := filepath.Join(cwd, "devlog", "_plan", "260821_partial")
	entries, err := os.ReadDir(unit)
	if err != nil || len(entries) != 1 || entries[0].Name() != "000_plan.md" {
		t.Fatal(entries, err)
	}
	if b, err := os.ReadFile(filepath.Join(unit, "000_plan.md")); err != nil || string(b) != planDoc(a.Slug) {
		t.Fatal(string(b), err)
	}
}

func TestPlanConcurrentCreation(t *testing.T) {
	cwd := t.TempDir()
	a := parsedPlan(t, cwd, "init", "260821_concurrent")
	start := make(chan struct{})
	results := make(chan PlanCliResult, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; results <- RunPlanCli(a) }()
	}
	close(start)
	wg.Wait()
	close(results)
	successes := 0
	for r := range results {
		if r.Code == 0 {
			successes++
		} else if !strings.Contains(r.Output, "already exists") {
			t.Error(r)
		}
	}
	if successes != 1 {
		t.Fatalf("successful creators = %d", successes)
	}
}

func TestPlanMissingLinkedAndEmptyDateWorkspaces(t *testing.T) {
	for _, kind := range []string{"missing", "linked", "empty-date"} {
		t.Run(kind, func(t *testing.T) {
			cwd := filepath.Join(t.TempDir(), "workspace")
			if kind == "linked" {
				if err := os.Symlink(t.TempDir(), cwd); err != nil {
					t.Fatal(err)
				}
			}
			date := "260821"
			if kind == "empty-date" {
				date = ""
			}
			successfulPlan(t, PlanCliArgs{Verb: "init", Slug: "unit", Phases: 1, Date: &date, Cwd: cwd})
			if names := planNames(t, cwd); !slices.Equal(names, []string{date + "_unit"}) {
				t.Fatal(names)
			}
		})
	}
}

func TestPlanOrdinaryErrorOracle(t *testing.T) {
	var rows []struct {
		Operation, Errno string
		Result           PlanCliResult
		Recording        string
	}
	raw, err := os.ReadFile("testdata/plan/errors-oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	for i, c := range rows {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			cwd := t.TempDir()
			a := parsedPlan(t, cwd, "init", "260821_unit")
			var got PlanCliResult
			if strings.HasPrefix(c.Recording, "Actual") {
				if c.Errno == "ENOTDIR" {
					if err := os.WriteFile(filepath.Join(cwd, "devlog"), []byte("original"), 0o644); err != nil {
						t.Fatal(err)
					}
				} else {
					if os.Geteuid() == 0 {
						t.Skip("permission case requires unprivileged user; injected row still runs")
					}
					if err := os.Chmod(cwd, 0o555); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = os.Chmod(cwd, 0o755) })
				}
				got = RunPlanCli(a)
			} else {
				errno := map[string]syscall.Errno{"ENOTDIR": syscall.ENOTDIR, "EACCES": syscall.EACCES, "EIO": syscall.EIO, "EEXIST": syscall.EEXIST}[c.Errno]
				got = runPlanCli(a, func(root *planDir, name, data string) error {
					path := filepath.Join(root.Name(), name)
					if c.Operation == "mkdir" {
						path = root.Name()
					}
					return &os.PathError{Op: c.Operation, Path: path, Err: errno}
				})
			}
			want := c.Result
			want.Output = strings.ReplaceAll(want.Output, "WORKSPACE", cwd)
			if got != want {
				t.Fatalf("result = %#v; want %#v", got, want)
			}
		})
	}
}

func TestPlanRelativeCwd(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	for _, cwd := range []string{"relative", ""} {
		a := parsedPlan(t, cwd, "init", "260821_unit")
		successfulPlan(t, a)
		r := RunPlanCli(a)
		abs, err := filepath.Abs(cwd)
		if err != nil {
			t.Fatal(err)
		}
		if r.Code != 1 || !strings.Contains(r.Output, filepath.Join(abs, "devlog", "_plan", "260821_unit")) {
			t.Fatal(r)
		}
	}
}

func TestPlanWriteErrorDoesNotOverwrite(t *testing.T) {
	root, err := openPlanWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.WriteFile(filepath.Join(root.Name(), "doc"), []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writePlanDoc(root, "doc", "new"); !errors.Is(err, os.ErrExist) {
		t.Fatalf("exclusive open: %v", err)
	}
}

func TestPlanSearchOnlyDirectoryOracle(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("unprivileged permission case")
	}
	var rows []struct {
		Part, Document string
		Result         PlanCliResult
	}
	raw, err := os.ReadFile("testdata/plan/search-oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	for _, c := range rows {
		t.Run(c.Part, func(t *testing.T) {
			cwd := t.TempDir()
			parent := filepath.Join(cwd, "devlog", "_plan")
			if err := os.MkdirAll(parent, 0o755); err != nil {
				t.Fatal(err)
			}
			limited := cwd
			if c.Part == "devlog" {
				limited = filepath.Join(cwd, "devlog")
			} else if c.Part == "_plan" {
				limited = parent
			}
			if err := os.Chmod(limited, 0o300); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(limited, 0o755) })
			r := RunPlanCli(parsedPlan(t, cwd, "init", "260821_unit"))
			if r != c.Result {
				t.Fatalf("search-only %s: %#v; want %#v", c.Part, r, c.Result)
			}
			if err := os.Chmod(limited, 0o755); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(filepath.Join(parent, "260821_unit", "000_plan.md"))
			if err != nil || string(b) != c.Document {
				t.Fatal(err)
			}
		})
	}
}

func TestPlanFIFOReplacement(t *testing.T) {
	if os.Getenv("CRW_PLAN_FIFO_PROBE") == "1" {
		cwd := t.TempDir()
		path := filepath.Join(cwd, "child")
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
		root, err := openPlanWorkspace(cwd)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		child, err := openPlanChildWith(root, "child", func() error {
			if err := os.Remove(path); err != nil {
				return err
			}
			return unix.Mkfifo(path, 0o600)
		})
		if child != nil {
			child.Close()
		}
		if err == nil {
			t.Fatal("accepted FIFO replacement")
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPlanFIFOReplacement$")
	cmd.Env = append(os.Environ(), "CRW_PLAN_FIFO_PROBE=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("FIFO replacement blocked or failed: %v; %s", err, out)
	}
}
