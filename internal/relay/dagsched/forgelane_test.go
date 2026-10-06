package dagsched

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/mergeturn"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// forgeLanePulls is the reader the merge lane is given in the forge world: the forge kit's tip reader, and the pull request head reader that lane Check compares the head it restates with. It answers
// for no forge and starts no gh. What it shows for a pull request is what a test seeded with show, keyed by the pull request alone: it never reads the merge turn, its candidate, the scheduler's scripted
// forge (k.forge.by) or a work report, so a lane that compares the wrong head fails here instead of agreeing with itself. A pull request nobody seeded is an error, with no fallback, and every pull
// request it was asked about is kept so that a test can say which one the lane compared.
type forgeLanePulls struct {
	*forgeKitReaders
	pullsMu sync.Mutex
	heads   map[string]string // "owner/repo#N" -> the head the pull request shows
	asked   []string          // the pull requests asked about, in order, since the last took
}

func newForgeLanePulls(readers *forgeKitReaders) *forgeLanePulls {
	return &forgeLanePulls{forgeKitReaders: readers, heads: map[string]string{}}
}

// lanePulls is the lane's reader over the forge world of the kit.
func (k *integrationKit) lanePulls() *forgeLanePulls {
	k.t.Helper()
	if k.readers == nil {
		k.t.Fatal("the local world has no forge readers to build the lane's pull request head reader on")
	}
	return newForgeLanePulls(k.readers)
}

func forgeLanePullKey(repository string, number int64) string {
	return fmt.Sprintf("%s#%d", repository, number)
}

// show says that pull request number of the kit's forge repository shows head now.
func (p *forgeLanePulls) show(number int64, head string) {
	p.pullsMu.Lock()
	defer p.pullsMu.Unlock()
	p.heads[forgeLanePullKey(forgeKitRepository, number)] = head
}

// PullRequestHead is the mergeturn.PullRequestHeadReader of the lane.
func (p *forgeLanePulls) PullRequestHead(_ context.Context, repository string, number int64) (mergeturn.PullRequestHeadReading, error) {
	p.pullsMu.Lock()
	defer p.pullsMu.Unlock()
	key := forgeLanePullKey(repository, number)
	p.asked = append(p.asked, key)
	head, ok := p.heads[key]
	if !ok {
		return mergeturn.PullRequestHeadReading{}, fmt.Errorf("the lane's fake shows no head for pull request %s: seed it with show", key)
	}
	return mergeturn.PullRequestHeadReading{Repository: repository, Number: number, SHA: head, Source: "fake:forgelane"}, nil
}

// took returns the pull requests asked about since the last call, and forgets them.
func (p *forgeLanePulls) took() []string {
	p.pullsMu.Lock()
	defer p.pullsMu.Unlock()
	out := p.asked
	p.asked = nil
	return out
}

// forgeLaneService is the merge lane of a scenario: the real service over the scenario's store, with the fake as the reader of pull request heads (Check reads the one it is given; Ready reads Pulls).
func forgeLaneService(sched *Scheduler, s *store.Store, pulls *forgeLanePulls) *mergeturn.Service {
	return &mergeturn.Service{Store: s, Registry: &registry.Registry{Store: s}, Now: sched.now, Delivery: mergeturn.StoreDelivery{Store: s}, Pulls: pulls}
}

// forgeLaneGreen is a review with nothing open.
func forgeLaneGreen() contract.OrderedObject {
	return contract.OrderedObject{{Key: "hasNextPage", Value: false}, {Key: "pagesRead", Value: json.Number("1")}, {Key: "totalCount", Value: json.Number("0")}, {Key: "threadsSeen", Value: []any{}}, {Key: "unresolved", Value: json.Number("0")}}
}

// forgeLaneChecks is the check list the lane restates for a pull request.
func forgeLaneChecks(checks []Check) []any {
	var list []any
	for _, c := range checks {
		list = append(list, contract.OrderedObject{{Key: "runId", Value: c.RunID}, {Key: "name", Value: c.Name}, {Key: "headSha", Value: c.HeadSHA}, {Key: "conclusion", Value: c.Conclusion}, {Key: "attempt", Value: json.Number(strconv.FormatInt(c.Attempt, 10))}})
	}
	return list
}

// forgeLaneCheck is the lane's own check of a turn on the forge, with the fake as the reader. It must pass, and it must have been decided by the forge: the answer names the head the pull request showed
// and the pull request number of the turn, and the fake was asked about that turn's repository and pull request and no other.
func forgeLaneCheck(t *testing.T, service *mergeturn.Service, pulls *forgeLanePulls, turn, head, base string, list []any, review any, required []string) map[string]any {
	t.Helper()
	pulls.took()
	answer, err := service.Check(context.Background(), turn, "parent", head, base, list, review, required, pulls)
	if err != nil {
		t.Fatalf("the lane's check of turn %s: %v", turn, err)
	}
	decided, _ := answer["pullRequestHead"].(map[string]any)
	number, _ := answer["prNumber"].(int64)
	if decided["decidedBy"] != "forge" || decided["head"] != head || decided["pullRequest"] != number || number == 0 {
		t.Fatalf("the head of turn %s was not decided by the forge pull request: %v (turn pull request %v)", turn, decided, answer["prNumber"])
	}
	asked := pulls.took()
	if want := forgeLanePullKey(fmt.Sprint(answer["repository"]), number); strings.Join(asked, ",") != want {
		t.Fatalf("the lane compared %v, want only the pull request %s of turn %s", asked, want, turn)
	}
	return answer
}

// The fake is what the lane consults, and it is independent of the turn: the turn's candidate is the head the pull request showed at acceptance, the head the call restates is the candidate, and only what the
// forge shows now can make the comparison fail. A pull request that shows another head is refused as another candidate, one nobody seeded is unreadable, and the pull request showing the candidate passes and
// is decided by the forge.
func TestLaneCheckComparesTheHeadWithThePullRequestTheForgeShows(t *testing.T) {
	t.Parallel()
	k := newLaneKit(t)
	ctx := context.Background()
	service := forgeLaneService(k.sched, k.s, k.pulls)
	_, turn, err := k.request("I")
	if err != nil || turn == nil {
		t.Fatalf("request = %v %v", err, turn)
	}
	id := turn["turnId"].(string)
	if grant, _ := turn["grant"].(map[string]any); grant != nil {
		if _, err := service.Acknowledge(ctx, id, "parent", grant["grantId"].(string), "re-read the store before merging"); err != nil {
			t.Fatalf("acknowledging the grant: %v", err)
		}
	}
	if turn["repository"] != forgeKitRepository || turn["prNumber"] != int64(5) || turn["candidateHead"] != k.heads["I"] {
		t.Fatalf("the turn = %v, want pull request 5 of the forge repository at the head accepted", turn)
	}
	base, list, review, required := k.repo.git("rev-parse", "dev"), forgeLaneChecks(k.prs["I"].Checks), forgeLaneGreen(), []string{"A", "B"}
	snapshot := func() string {
		var state, candidate string
		if err := k.s.DB.QueryRow("SELECT state, candidate_head FROM merge_turns WHERE turn_id = ?", id).Scan(&state, &candidate); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%s %s ledger=%d", state, candidate, k.count("SELECT COUNT(*) FROM merge_turn_ledger WHERE turn_id = ?", id))
	}
	before := snapshot()

	// the pull request shows the head of another one now; the call restates the turn's own candidate unchanged, so only the forge's answer can refuse it
	k.pulls.show(5, k.heads["D"])
	if _, err := service.Check(ctx, id, "parent", k.heads["I"], base, list, review, required, k.pulls); refusalReason(err) != "merge_candidate_moved" || !strings.Contains(err.Error(), k.heads["D"]) {
		t.Fatalf("check while the pull request shows another head = %v, want merge_candidate_moved naming the head it shows", err)
	}
	if got := snapshot(); got != before {
		t.Fatalf("the refused check changed the turn: %s -> %s", before, got)
	}
	// nothing is shown for the pull request: the head cannot be compared, which is its own refusal
	if _, err := service.Check(ctx, id, "parent", k.heads["I"], base, list, review, required, newForgeLanePulls(k.readers)); refusalReason(err) != "merge_target_unreadable" {
		t.Fatalf("check with a pull request nobody seeded = %v, want merge_target_unreadable", err)
	}
	if got := snapshot(); got != before {
		t.Fatalf("the unreadable check changed the turn: %s -> %s", before, got)
	}
	// the pull request shows the candidate: the lane begins the merge, and the forge decided the head
	k.pulls.show(5, k.heads["I"])
	answer := forgeLaneCheck(t, service, k.pulls, id, k.heads["I"], base, list, review, required)
	if answer["state"] != mergeturn.Merging {
		t.Fatalf("the turn after a check the forge agreed with = %v", answer["state"])
	}
}
