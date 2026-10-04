package goalplan

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// The oracle's answers for the cases of testdata/oracle-plan.json were recorded once with testdata/record-oracle-plan.mjs (CXC
// v0.2.40 under Node 24); no Node runs here. A plan case holds the raw text of a goalplan.json and the compact JSON the oracle's
// readGoalplan revived it to (null when it refused), a slug case what validateGoalplanSlug answered, a dir case what goalplanDir
// answered. The oracle's answers name ".codexclaw"; the replay maps it to crwdir.DirName.

type recordedPlan struct {
	Raw    string  `json:"raw"`
	Slug   string  `json:"slug"`
	Oracle *string `json:"oracle"`
}

type recordedAnswer struct {
	Input  string `json:"input"`
	Cwd    string `json:"cwd"`
	Slug   string `json:"slug"`
	Oracle string `json:"oracle"`
}

type oraclePlans struct {
	Plans map[string]recordedPlan   `json:"plans"`
	Slugs map[string]recordedAnswer `json:"slugs"`
	Dirs  map[string]recordedAnswer `json:"dirs"`
}

func loadOraclePlans(t *testing.T) oraclePlans {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "oracle-plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	var o oraclePlans
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatal(err)
	}
	if len(o.Plans) < 400 || len(o.Slugs) < 30 || len(o.Dirs) < 5 {
		t.Fatalf("%d plan, %d slug, %d dir cases recorded", len(o.Plans), len(o.Slugs), len(o.Dirs))
	}
	return o
}

// decodePlan is JSON.parse as a plan reader does it: numbers stay json.Number.
func decodePlan(t *testing.T, raw string) any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// revivedText is the compact JSON of what reviveGoalplan makes of raw, "null" when it refuses.
func revivedText(t *testing.T, raw, slug string) string {
	t.Helper()
	return compact(t, reviveGoalplan(decodePlan(t, raw), &slug))
}

func TestOraclePlanParity(t *testing.T) {
	o := loadOraclePlans(t)
	for _, name := range slices.Sorted(maps.Keys(o.Plans)) {
		c := o.Plans[name]
		t.Run(name, func(t *testing.T) {
			want := "null"
			if c.Oracle != nil {
				want = *c.Oracle
			}
			got := revivedText(t, c.Raw, c.Slug)
			if got != want {
				t.Fatalf("revived\n%s\nwant\n%s", got, want)
			}
			// What a revived plan holds survives being written and read back: reviving it again changes nothing.
			if again := revivedText(t, got, c.Slug); again != got {
				t.Fatalf("reviving the revived record again gave\n%s\nwant\n%s", again, got)
			}
		})
	}
}

func TestOracleSlugAndDirParity(t *testing.T) {
	o := loadOraclePlans(t)
	for _, name := range slices.Sorted(maps.Keys(o.Slugs)) {
		c := o.Slugs[name]
		t.Run("slug_"+name, func(t *testing.T) {
			got, err := ValidateGoalplanSlug(c.Input)
			answer := "ok:" + got
			if err != nil {
				answer = "err:" + err.Error()
			}
			if answer != c.Oracle {
				t.Fatalf("ValidateGoalplanSlug(%q) = %q, oracle %q", c.Input, answer, c.Oracle)
			}
		})
	}
	for _, name := range slices.Sorted(maps.Keys(o.Dirs)) {
		c := o.Dirs[name]
		t.Run("dir_"+name, func(t *testing.T) {
			got, err := GoalplanDir(c.Cwd, c.Slug)
			answer := "ok:" + got
			if err != nil {
				answer = "err:" + err.Error()
			}
			if want := strings.ReplaceAll(c.Oracle, "/.codexclaw/", "/"+crwdir.DirName+"/"); answer != want {
				t.Fatalf("GoalplanDir(%q, %q) = %q, oracle %q", c.Cwd, c.Slug, answer, want)
			}
		})
	}
}
