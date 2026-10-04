package recall

import (
	"crypto/sha256"
	"encoding/json"
	"math"
	"os"
	"reflect"
	"slices"
	"strconv"
	"testing"
)

type jsSortParameters struct {
	Kind    string
	N       int
	Seed    uint32
	Lengths []int
	Score   json.RawMessage
	Date    *string
}

type memoryTrimOracleCase struct {
	Name  string
	Gen   jsSortParameters
	Input []struct {
		Score     json.RawMessage
		UpdatedAt *string
	}
	Limit       json.RawMessage
	Out, Sorted []int
}

type jsSortOracle struct {
	Format   int
	Node, V8 string
	Seed0    uint32
	LCG      [2]uint32
	Sorts    []struct {
		Name, Mode  string
		Gen         jsSortParameters
		Out         []int
		Trace       [][2]int
		TraceCount  int
		TraceDigest [32]byte
	}
	Snapshot struct {
		Input, Out []int
		Visible    [][]int
	}
	Ranks []memoryTrimOracleCase
}

func jsSortReadOracle(t *testing.T) jsSortOracle {
	t.Helper()
	b, err := os.ReadFile("testdata/jssort/oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > 1_000_000 {
		t.Fatalf("recorded oracle is %d bytes, limit 1000000", len(b))
	}
	var grid jsSortOracle
	if err := json.Unmarshal(b, &grid); err != nil {
		t.Fatal(err)
	}
	if grid.Format != 2 || grid.Seed0 != 526 || grid.LCG != [2]uint32{1664525, 1013904223} || grid.Node != "24.20.0" || grid.V8 != "13.6.233.17-node.53" || len(grid.Sorts) != 442 || len(grid.Ranks) != 350 {
		t.Fatalf("unexpected oracle identity/counts: %s %s sorts=%d ranks=%d", grid.Node, grid.V8, len(grid.Sorts), len(grid.Ranks))
	}
	full := 0
	for _, c := range grid.Sorts {
		if c.Trace != nil {
			full++
		}
	}
	if full != 124 {
		t.Fatalf("got %d full witness traces, want 124", full)
	}
	return grid
}

func jsSortNumber(t *testing.T, raw json.RawMessage) float64 {
	t.Helper()
	var value float64
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatal(err)
		}
		switch s {
		case "nan":
			return math.NaN()
		case "infinity":
			return math.Inf(1)
		case "-infinity":
			return math.Inf(-1)
		default:
			v, err := strconv.ParseFloat(s, 64)
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func (g *jsSortParameters) next() uint32 {
	g.Seed = g.Seed*1664525 + 1013904223 // Math.imul(...)+c >>> 0.
	return g.Seed
}

func jsSortNumbers() []float64 {
	return []float64{math.Inf(-1), -2, -1, math.Copysign(0, -1), 0, 1, 2, math.Inf(1), math.NaN()}
}

// Each seed is captured before this case consumes any draws. Tables consume
// 169 row-major draws before Fisher-Yates; runs consume none.
func jsSortGenerate(t *testing.T, g jsSortParameters) ([]int, [][]float64) {
	t.Helper()
	if g.Kind == "runs" {
		offset, out := 0, []int{}
		for _, n := range g.Lengths {
			offset += n
		}
		for _, n := range g.Lengths {
			offset -= n
			for i := range n {
				out = append(out, offset+i)
			}
		}
		return out, nil
	}
	var table [][]float64
	if g.Kind == "table" {
		numbers := jsSortNumbers()
		for range 13 {
			row := make([]float64, 13)
			for i := range row {
				row[i] = numbers[g.next()%uint32(len(numbers))]
			}
			table = append(table, row)
		}
	} else if g.Kind != "shuffle" {
		t.Fatalf("unknown sort generator %q", g.Kind)
	}
	out := make([]int, g.N)
	for i := range out {
		out[i] = i
	}
	for i := len(out) - 1; i > 0; i-- {
		j := int(g.next() % uint32(i+1))
		out[i], out[j] = out[j], out[i]
	}
	return out, table
}

func TestJSSortNodeOracle(t *testing.T) {
	for _, c := range jsSortReadOracle(t).Sorts {
		t.Run(c.Name, func(t *testing.T) {
			values, table := jsSortGenerate(t, c.Gen)
			trace := [][2]int{} // JSON [] matches Node; nil would encode null.
			calls := 0
			JSSort(values, func(a, b int) float64 {
				trace = append(trace, [2]int{a, b})
				calls++
				switch c.Mode {
				case "asc":
					return float64(a - b)
				case "desc":
					return float64(b - a)
				case "zero":
					return math.Copysign(0, -1)
				case "negative":
					return -1
				case "positive":
					return 1
				case "nan":
					return math.NaN()
				case "cycle":
					if a%3 == b%3 {
						return 0
					}
					if (a+1)%3 == b%3 {
						return -1
					}
					return 1
				case "table":
					return table[a%len(table)][b%len(table)]
				case "stateful":
					if calls%7 == 0 {
						return math.NaN()
					}
					if calls%3 == 0 {
						return 0
					}
					if calls%2 != 0 {
						return -1
					}
					return 1
				default:
					t.Fatalf("unknown comparator %s", c.Mode)
					return 0
				}
			})
			if !slices.Equal(values, c.Out) {
				t.Errorf("output differs from Node: got %v, want %v", values, c.Out)
			}
			if len(trace) != c.TraceCount {
				t.Errorf("comparison count differs: got %d, want %d", len(trace), c.TraceCount)
			}
			if c.Trace != nil {
				if !slices.Equal(trace, c.Trace) {
					t.Error("full comparison schedule differs from Node")
				}
			} else {
				b, err := json.Marshal(trace)
				if err != nil {
					t.Fatal(err)
				}
				if sha256.Sum256(b) != c.TraceDigest {
					t.Error("canonical comparison schedule digest differs from Node")
				}
			}
		})
	}
}

func TestJSSortReceiverSnapshot(t *testing.T) {
	c := jsSortReadOracle(t).Snapshot
	values, visible := slices.Clone(c.Input), [][]int{}
	JSSort(values, func(a, b int) float64 {
		visible = append(visible, slices.Clone(values))
		return float64(a - b)
	})
	if !slices.Equal(values, c.Out) || !reflect.DeepEqual(visible, c.Visible) {
		t.Fatal("receiver visibility differs from Node")
	}
}
