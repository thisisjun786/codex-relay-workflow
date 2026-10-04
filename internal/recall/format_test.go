package recall

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type formatOracleRow struct {
	oracleCase
	Error, Stderr, Classification, Reason string
}

func formatOracleRows(t *testing.T) []formatOracleRow {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "format", "oracle.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rows []formatOracleRow
	if err := json.Unmarshal(b, &rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

// The six upstream format-freshness.test.ts cases are covered by age, memory,
// and newer rows. Whole formatted strings, not fragments, are pinned by Node.
func TestFormatOracle(t *testing.T) {
	t.Setenv("TZ", "UTC")
	old := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = old })
	seen := map[string]int{}
	for i, c := range formatOracleRows(t) {
		var got any
		switch c.Fn {
		case "memory":
			got = FormatMemoryResult(arg[MemorySearchResult](t, c.oracleCase, 0), arg[float64](t, c.oracleCase, 1))
		case "age":
			if len(c.In) > 2 {
				zone := arg[string](t, c.oracleCase, 2)
				loc, e := time.LoadLocation(zone)
				if e != nil {
					t.Fatal(e)
				}
				time.Local = loc
			} else {
				time.Local = time.UTC
			}
			got = AgeDays(arg[*string](t, c.oracleCase, 0), arg[float64](t, c.oracleCase, 1))
		case "newer":
			got = NewerRelpath(arg[MemoryHit](t, c.oracleCase, 0), arg[[]MemoryHit](t, c.oracleCase, 1))
		case "clip":
			got = clip(arg[string](t, c.oracleCase, 0), arg[int](t, c.oracleCase, 1))
		case "topics":
			got = topicTokens(arg[MemoryHit](t, c.oracleCase, 0))
		case "header":
			got = chatHitHeader(arg[ChatHit](t, c.oracleCase, 0))
		case "chat":
			got = FormatChatResult(arg[ChatSearchResult](t, c.oracleCase, 0))
		case "json":
			got = ClipChatResultForJson(arg[ChatSearchResult](t, c.oracleCase, 0))
		default:
			continue
		}
		seen[c.Fn]++
		var want any
		if err := json.Unmarshal(c.Out, &want); err != nil {
			t.Fatal(err)
		}
		if actual := canon(t, got); !reflect.DeepEqual(actual, want) {
			t.Errorf("case %d %s: got %.500v; oracle %.500v", i, c.Fn, actual, want)
		}
	}
	if len(seen) != 8 {
		t.Fatalf("replayed %d format functions, want 8", len(seen))
	}
	t.Logf("replayed format oracle groups: %v", seen)
}

func TestFormatClippingDoesNotMutate(t *testing.T) {
	title := strings.Repeat("t", 501)
	r := ChatSearchResult{Hits: []ChatHit{{Text: strings.Repeat("x", 501), Title: &title, Context: []ChatContextEntry{{Text: strings.Repeat("c", 501), IsMatch: true}}}}, Warnings: []string{"warning"}, Index: &ChatIndexInfo{Files: 2}}
	before := canon(t, r)
	got := ClipChatResultForJson(r)
	if !got.Clipped || len(got.Hits[0].Text) != 503 || len(*got.Hits[0].Title) != 503 || len(got.Hits[0].Context[0].Text) != 503 {
		t.Fatalf("clipped = %#v", got)
	}
	got.Hits[0].Text = "changed"
	got.Hits[0].Context[0].Text = "changed"
	*got.Hits[0].Title = "changed"
	if !reflect.DeepEqual(canon(t, r), before) {
		t.Fatal("clipping mutated source hits/title/context")
	}
	if got.Index != r.Index || !reflect.DeepEqual(got.Warnings, r.Warnings) {
		t.Fatal("result envelope was not preserved")
	}
	empty := ClipChatResultForJson(ChatSearchResult{Hits: []ChatHit{}, Warnings: []string{}})
	if empty.Hits == nil || empty.Clipped {
		t.Fatal("empty map result must be [], unclipped")
	}
}

func TestFormatMemoryPreservesOrderAndDefaultClock(t *testing.T) {
	hits := []MemoryHit{{Relpath: "old.md", Excerpt: "2.49.0", UpdatedAt: formatString("2026-08-27T00:00:00.000Z"), Score: 9}, {Relpath: "new.md", Excerpt: "2.49.0", UpdatedAt: formatString("2026-09-09T00:00:00.000Z"), Score: 3}}
	before := canon(t, hits)
	s := FormatMemoryResult(MemorySearchResult{Hits: hits}, 1788998400000)
	if !reflect.DeepEqual(canon(t, hits), before) || !strings.Contains(s, "[newer: new.md]") {
		t.Fatal("labels reordered or mutated hits", s)
	}
	if !strings.Contains(FormatMemoryResult(MemorySearchResult{Hits: []MemoryHit{{UpdatedAt: formatString(time.Now().UTC().Format(time.RFC3339))}}}), "[age: 0d]") {
		t.Fatal("default clock not applied")
	}
}
func formatString(s string) *string { return &s }

func TestFormatFreshness(t *testing.T) {
	old := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = old })
	names := []string{"age_whole_days_invalid_future", "without_timestamp", "newer_same_topic_preserves_order", "no_distinctive_topic", "same_file_not_correction", "envelope_and_empty"}
	rows := formatOracleRows(t)
	for i, name := range names {
		t.Run(name, func(t *testing.T) {
			time.Local = time.UTC
			for n, c := range rows {
				selected := i == 0 && c.Fn == "age" || i > 0 && c.Fn == "memory" && n == i-1 || i == 5 && c.Fn == "memory" && n == 5
				if !selected {
					continue
				}
				var got any
				if c.Fn == "age" {
					if len(c.In) > 2 {
						zone := arg[string](t, c.oracleCase, 2)
						loc, e := time.LoadLocation(zone)
						if e != nil {
							t.Fatal(e)
						}
						time.Local = loc
					} else {
						time.Local = time.UTC
					}
					got = AgeDays(arg[*string](t, c.oracleCase, 0), arg[float64](t, c.oracleCase, 1))
				} else {
					got = FormatMemoryResult(arg[MemorySearchResult](t, c.oracleCase, 0), arg[float64](t, c.oracleCase, 1))
				}
				var want any
				if err := json.Unmarshal(c.Out, &want); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(canon(t, got), want) {
					t.Errorf("freshness %s differs: %.500v vs %.500v", name, got, want)
				}
			}
		})
	}
}
