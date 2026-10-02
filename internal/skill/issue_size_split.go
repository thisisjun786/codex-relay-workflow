package skill

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// The split proposal for an issue that is split_recommended. It is a draft for crw-plan to judge, and
// it is built from counts and word overlap only, with integer arithmetic, so the same issue gives the
// same bytes on every platform: nothing here is a float, a random choice or a map order.
//
// Completion criteria keep the order the issue states them in and are cut into contiguous, near-equal
// groups. A research-reinforcement row joins the group whose criteria share the most wording with it,
// or, when the planner declared what it needs (depends_on), the latest group among those needs.
// Bundles are ordered by what the planner declared they need and by the files they would change: by
// the regions the criteria name, a bundle that names none counting as overlapping every other one, the
// way the DAG scheduler reads an unknown edit region. A prerequisite is never derived from the prose.

const (
	bundleTarget   = 4 // criteria a bundle is sized for: half of the criteria_total limit
	bundleMin      = 2
	bundleMax      = 5
	criterionLimit = 100 // runes of a criterion's text kept in the draft
)

const proposalNote = "Draft only. Bundles keep the order of the issue's criteria; a research-reinforcement row joins the bundle whose criteria share the most wording with it, or the latest bundle among the criteria the planner declared it needs. A bundle becomes its own issue only where the crw-plan boundary rules hold (an observable result of its own, verifiable apart or landing after a prerequisite). Order edges come from the prerequisites the planner declared and from the regions the criteria name, with unknown regions counted as overlapping; crw-plan decides the real relations."

const noSplitNote = "There is nothing to divide: the issue has fewer than two criteria. Where it declares several deliverables, crw-plan divides the criterion itself under the boundary rules."

type splitProposal struct {
	Status  string      `json:"status"`
	Note    string      `json:"note"`
	Bundles []bundle    `json:"bundles"`
	Order   []orderEdge `json:"order"`
}

type bundle struct {
	ID            string            `json:"id"`
	Criteria      []bundleCriterion `json:"criteria"`
	Regions       []string          `json:"regions"`
	CriteriaTotal int               `json:"criteria_total"`
}

type bundleCriterion struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type orderEdge struct {
	From    string   `json:"from"`
	To      string   `json:"to"`
	Reason  string   `json:"reason"` // prerequisite, shared_prefix or unknown_regions
	Regions []string `json:"regions,omitempty"`
	Needs   []string `json:"needs,omitempty"` // for a prerequisite edge: "C4 needs C1"
}

// criterion is one item of the issue: C<n> for the n-th completion criterion, R<n> for the n-th research row.
type criterion struct {
	id, text string
	grams    map[string]bool
}

// bigrams are the pairs of adjacent letters or digits in each word of the text, lowercased.
func bigrams(text string) map[string]bool {
	set := map[string]bool{}
	for _, word := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) }) {
		runes := []rune(word)
		for i := 0; i+1 < len(runes); i++ {
			set[string(runes[i:i+2])] = true
		}
	}
	return set
}

func shared(a, b map[string]bool) int {
	n := 0
	for g := range a {
		if b[g] {
			n++
		}
	}
	return n
}

func shortText(text string) string {
	words := strings.Join(strings.Fields(text), " ")
	if runes := []rune(words); len(runes) > criterionLimit {
		return string(runes[:criterionLimit]) + "…"
	}
	return words
}

// proposeSplit drafts the bundles. An issue with fewer than two criteria has nothing to divide: its
// proposal says so, with no bundles, rather than being absent. depends has been checked against the
// items (checkDepends).
func proposeSplit(completion, research []string, depends map[string][]string) *splitProposal {
	total := len(completion) + len(research)
	if total < 2 {
		return &splitProposal{Status: "none", Note: noSplitNote, Bundles: []bundle{}, Order: []orderEdge{}}
	}
	var all []criterion
	index := map[string]int{}
	for i, text := range completion {
		index[fmt.Sprintf("C%d", i+1)] = len(all)
		all = append(all, criterion{fmt.Sprintf("C%d", i+1), text, bigrams(text)})
	}
	for i, text := range research {
		index[fmt.Sprintf("R%d", i+1)] = len(all)
		all = append(all, criterion{fmt.Sprintf("R%d", i+1), text, bigrams(text)})
	}
	// Completion criteria seed the bundles; with fewer than two of them every item is a seed and no row is placed by wording.
	seeds, rows := len(completion), len(research)
	if seeds < 2 {
		seeds, rows = total, 0
	}
	count := min(max(bundleMin, min(bundleMax, (total+bundleTarget-1)/bundleTarget)), seeds)
	members := make([][]int, count)
	groupOf := make([]int, total)
	for g, next := 0, 0; g < count; g++ {
		size := seeds / count
		if g < seeds%count {
			size++
		}
		for ; size > 0; size-- {
			members[g] = append(members[g], next)
			groupOf[next] = g
			next++
		}
	}
	placed := make([]bool, rows)
	for r := 0; r < rows; r++ {
		if needs := depends[all[seeds+r].id]; len(needs) > 0 {
			g := 0
			for _, need := range needs {
				g = max(g, groupOf[index[need]])
			}
			members[g] = append(members[g], seeds+r)
			groupOf[seeds+r] = g
			placed[r] = true
		}
	}
	capacity := (total + count - 1) / count
	type pair struct{ row, group, score int }
	var pairs []pair
	for r := 0; r < rows; r++ {
		if placed[r] {
			continue
		}
		for g := range members {
			best := 0
			for _, c := range members[g] {
				if c < seeds {
					best = max(best, shared(all[seeds+r].grams, all[c].grams))
				}
			}
			pairs = append(pairs, pair{r, g, best})
		}
	}
	sort.Slice(pairs, func(a, b int) bool {
		x, y := pairs[a], pairs[b]
		if x.score != y.score {
			return x.score > y.score
		}
		if x.row != y.row {
			return x.row < y.row
		}
		return x.group < y.group
	})
	for _, p := range pairs {
		if !placed[p.row] && len(members[p.group]) < capacity {
			placed[p.row] = true
			members[p.group] = append(members[p.group], seeds+p.row)
			groupOf[seeds+p.row] = p.group
		}
	}
	proposal := &splitProposal{Status: "draft", Note: proposalNote, Bundles: make([]bundle, count), Order: []orderEdge{}}
	for g := range members {
		sort.Ints(members[g])
		b := bundle{ID: fmt.Sprintf("B%d", g+1), Criteria: []bundleCriterion{}, CriteriaTotal: len(members[g])}
		var texts []string
		for _, i := range members[g] {
			b.Criteria = append(b.Criteria, bundleCriterion{all[i].id, shortText(all[i].text)})
			texts = append(texts, all[i].text)
		}
		b.Regions = pathRegions(strings.Join(texts, "\n"))
		proposal.Bundles[g] = b
	}
	needs := map[[2]int][]string{} // bundle pair -> "C4 needs C1"
	for key, list := range depends {
		for _, need := range list {
			from, to := groupOf[index[need]], groupOf[index[key]]
			if from != to {
				needs[[2]int{from, to}] = append(needs[[2]int{from, to}], key+" needs "+need)
			}
		}
	}
	proposal.Order = orderBundles(proposal.Bundles, needs)
	return proposal
}

func regionsOverlap(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

func shorter(a, b string) string {
	if len(a) <= len(b) {
		return a
	}
	return b
}

// orderBundles is the edges between bundles, each from the earlier bundle to the later: every pair the
// planner declared a prerequisite between, then the pairs that would change the same files unless the
// edges kept already reach the later bundle from the earlier one.
func orderBundles(bundles []bundle, needs map[[2]int][]string) []orderEdge {
	n := len(bundles)
	type candidate struct {
		from, to int
		edge     orderEdge
	}
	reach := make([][]bool, n)
	for i := range reach {
		reach[i] = make([]bool, n)
	}
	keep := func(from, to int) {
		reach[from][to] = true
		for a := 0; a < n; a++ {
			for b := 0; b < n; b++ {
				if (a == from || reach[a][from]) && (b == to || reach[to][b]) {
					reach[a][b] = true
				}
			}
		}
	}
	var kept []candidate
	for pair, why := range needs {
		sort.Strings(why)
		kept = append(kept, candidate{pair[0], pair[1], orderEdge{From: bundles[pair[0]].ID, To: bundles[pair[1]].ID, Reason: "prerequisite", Needs: why}})
	}
	for _, c := range kept {
		keep(c.from, c.to)
	}
	var candidates []candidate
	for from := 0; from < n; from++ {
		for to := from + 1; to < n; to++ {
			if _, declared := needs[[2]int{from, to}]; declared {
				continue
			}
			a, b := bundles[from], bundles[to]
			edge := orderEdge{From: a.ID, To: b.ID}
			if len(a.Regions) == 0 || len(b.Regions) == 0 {
				edge.Reason = "unknown_regions"
			} else {
				seen := map[string]bool{}
				for _, x := range a.Regions {
					for _, y := range b.Regions {
						if regionsOverlap(x, y) {
							seen[shorter(x, y)] = true
						}
					}
				}
				if len(seen) == 0 {
					continue
				}
				edge.Reason = "shared_prefix"
				for r := range seen {
					edge.Regions = append(edge.Regions, r)
				}
				sort.Strings(edge.Regions)
			}
			candidates = append(candidates, candidate{from, to, edge})
		}
	}
	// Shortest spans first, so an edge is kept only when the edges already kept do not reach its end.
	sort.Slice(candidates, func(a, b int) bool {
		x, y := candidates[a], candidates[b]
		if x.to-x.from != y.to-y.from {
			return x.to-x.from < y.to-y.from
		}
		return x.from < y.from
	})
	for _, c := range candidates {
		if !reach[c.from][c.to] {
			kept = append(kept, c)
			keep(c.from, c.to)
		}
	}
	sort.Slice(kept, func(a, b int) bool {
		if kept[a].from != kept[b].from {
			return kept[a].from < kept[b].from
		}
		return kept[a].to < kept[b].to
	})
	out := []orderEdge{}
	for _, c := range kept {
		out = append(out, c.edge)
	}
	return out
}
