package delivery

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// test_criteria_registration.py CRR-1..CRR-7.

const crrSource = "https://linear.app/doc/1"

func crrSet() []any {
	return []any{Obj{{Key: "id", Value: " c2 "}, {Key: "title", Value: " malformed requests are refused "}, {Key: "required", Value: int64(0)}}, Obj{{Key: "id", Value: "c1"}, {Key: "title", Value: "the endpoint returns the agreed shape"}}}
}

func crit(title string, required any) Obj {
	o := Obj{{Key: "title", Value: title}}
	if required != nil {
		o = append(o, F{Key: "required", Value: required})
	}
	return o
}

func withID(id string, o Obj) Obj { return append(Obj{{Key: "id", Value: id}}, o...) }

func runCRR(t *testing.T, mode string, goSide func(f *fixture, c *Criteria, out map[string]any)) {
	tree := t.TempDir()
	python := runPython(t, tree, "crr", mode)
	f := newFixture(t, tree)
	c := &Criteria{Store: f.store, Clock: f.clock}
	out := map[string]any{}
	goSide(f, c, out)
	for k, want := range python.Out {
		requireSameJSON(t, mode+"."+k, out[k], want)
	}
	requireSameTables(t, f, python)
}

func TestCRR01_an_empty_or_duplicate_set_is_refused_before_any_row(t *testing.T) {
	runCRR(t, "refused", func(f *fixture, c *Criteria, out map[string]any) {
		var results []any
		for _, entries := range [][]any{{}, {withID("c1", crit("   ", nil))}, {withID("c1", crit("one", nil)), "not-an-object"}, {withID("c1", crit("one", nil)), withID(" c1 ", crit("again", nil))}} {
			_, err := c.EnsureRegistered(f.ctx, "rel-1", entries, crrSource)
			requireReason(t, err, CriteriaUnregistered)
			results = append(results, refusalOf(err))
		}
		out["r"] = results
		out["mode"], _ = c.Mode(f.ctx, "rel-1")
	})
}

func TestCRR02_the_first_registration_normalises_and_returns_the_managed_set(t *testing.T) {
	runCRR(t, "first", func(f *fixture, c *Criteria, out map[string]any) {
		set := crrSet()
		r, err := c.EnsureRegistered(f.ctx, "rel-1", []any{set[1], set[0]}, crrSource)
		mustDo(t, err)
		out["r"] = r
	})
}

func TestCRR03_an_exact_replay_keeps_the_timestamp_and_writes_no_journal(t *testing.T) {
	runCRR(t, "replay", func(f *fixture, c *Criteria, out map[string]any) {
		_, err := c.EnsureRegistered(f.ctx, "rel-1", crrSet(), crrSource)
		mustDo(t, err)
		f.clock.Advance(30)
		r, err := c.EnsureRegistered(f.ctx, "rel-1", []any{withID("c2", crit("malformed requests are refused", false)), withID("c1", crit(" the endpoint returns the agreed shape ", nil))}, crrSource)
		mustDo(t, err)
		out["r"] = r
	})
}

func TestCRR04_a_changed_title_source_or_requirement_refuses_without_rewriting(t *testing.T) {
	runCRR(t, "changed", func(f *fixture, c *Criteria, out map[string]any) {
		_, err := c.EnsureRegistered(f.ctx, "rel-1", crrSet(), crrSource)
		mustDo(t, err)
		var results []any
		for _, tc := range []struct {
			entries []any
			source  string
		}{
			{[]any{withID("c1", crit("the endpoint returns the agreed shape", nil)), withID("c2", crit("a different refusal", false))}, crrSource},
			{crrSet(), "https://linear.app/doc/2"},
			{[]any{withID("c2", crit("malformed requests are refused", true)), withID("c1", crit("the endpoint returns the agreed shape", nil))}, crrSource},
			{append(crrSet(), withID("c3", crit("one more obligation", nil))), crrSource},
		} {
			_, err := c.EnsureRegistered(f.ctx, "rel-1", tc.entries, tc.source)
			requireReason(t, err, CriteriaSetChanged)
			results = append(results, refusalOf(err))
		}
		out["r"] = results
	})
}

func TestCRR05_register_still_replaces_a_set_ensure_would_keep(t *testing.T) {
	runCRR(t, "replace", func(f *fixture, c *Criteria, out map[string]any) {
		_, err := c.EnsureRegistered(f.ctx, "rel-1", crrSet(), crrSource)
		mustDo(t, err)
		f.clock.Advance(5)
		r, err := c.Register(f.ctx, "rel-1", []any{withID("c9", crit("the replacement obligation", false))}, "operator")
		mustDo(t, err)
		out["r"] = r
		_, err = c.EnsureRegistered(f.ctx, "rel-1", crrSet(), crrSource)
		out["r2"] = refusalOf(err)
	})
}

func TestCRR06_a_corrupt_or_mistyped_stored_set_is_refused_and_left(t *testing.T) {
	for _, mode := range []string{"corrupt", "required2"} {
		t.Run(mode, func(t *testing.T) {
			runCRR(t, mode, func(f *fixture, c *Criteria, out map[string]any) {
				normalised := []Criterion{{"c1", "the endpoint returns the agreed shape", true}, {"c2", "malformed requests are refused", false}}
				d, now := SetDigest(normalised), f.clock.ISO()
				mustDo(t, f.store.Transaction(f.ctx, func(ctx context.Context, _ *sql.Conn) error {
					rows := [][]any{{"rel-1", "c1", normalised[0].Title, 1, crrSource, d, now}, {"rel-1", "c2", normalised[1].Title, 0, "other-source", "not-the-digest", now}}
					mode2 := "legacy"
					if mode == "required2" {
						rows = [][]any{{"rel-1", "c1", normalised[0].Title, 2, crrSource, d, now}, {"rel-1", "c2", normalised[1].Title, 0, crrSource, d, now}}
						mode2 = "managed"
					}
					for _, r := range rows {
						if _, err := execSQL(ctx, f.store, "INSERT INTO canonical_criteria VALUES (?,?,?,?,?,?,?)", r...); err != nil {
							return err
						}
					}
					_, err := execSQL(ctx, f.store, "INSERT INTO verification_mode VALUES (?,?,?)", "rel-1", mode2, now)
					return err
				}))
				_, err := c.EnsureRegistered(f.ctx, "rel-1", crrSet(), crrSource)
				requireReason(t, err, CriteriaSetChanged)
				out["r"] = refusalOf(err)
			})
		})
	}
}

func TestCRR07_two_connections_register_exactly_one_set(t *testing.T) {
	for _, same := range []bool{false, true} {
		name := map[bool]string{false: "different sets", true: "same set"}[same]
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, "")
			path := filepath.Join(f.tree, "gostate", "relay.sqlite3")
			start := make(chan struct{})
			var wg sync.WaitGroup
			var mu sync.Mutex
			var refusals []string
			for i := 0; i < 2; i++ {
				entries, source := crrSet(), crrSource
				if i == 1 && !same {
					entries, source = []any{withID("c9", crit("a rival obligation", nil))}, "rival"
				}
				s, err := store.Open(f.ctx, path, "")
				mustDo(t, err)
				t.Cleanup(func() { _ = s.Close() })
				clock := NewFakeClock()
				clock.Advance(float64(i * 7))
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					_, err := (&Criteria{Store: s, Clock: clock}).EnsureRegistered(context.Background(), "rel-1", entries, source)
					if err != nil {
						mu.Lock()
						refusals = append(refusals, Reason(err))
						mu.Unlock()
					}
				}()
			}
			close(start)
			wg.Wait()
			digests := f.count("SELECT COUNT(DISTINCT set_digest) AS c FROM canonical_criteria")
			stamps := f.count("SELECT COUNT(DISTINCT recorded_at) AS c FROM canonical_criteria")
			journals := f.count("SELECT COUNT(*) AS c FROM journal WHERE subject = 'rel-1'")
			if digests != 1 || stamps != 1 || journals != 1 {
				t.Fatalf("digests %d stamps %d journals %d", digests, stamps, journals)
			}
			if same && len(refusals) != 0 || !same && (len(refusals) != 1 || refusals[0] != CriteriaSetChanged) {
				t.Fatalf("refusals %v", refusals)
			}
		})
	}
}
