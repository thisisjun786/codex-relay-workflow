package mergeturn

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// PullRequestHeadReading is where a pull request's head points on the forge now.
type PullRequestHeadReading struct {
	Repository string
	Number     int64
	SHA        string
	Source     string
}

// PullRequestHeadReader reads the head of one pull request. TargetReader is the production reader; a Reader that
// also implements it lets merge-turn-check compare the head it is given with the pull request's.
type PullRequestHeadReader interface {
	PullRequestHead(ctx context.Context, repository string, number int64) (PullRequestHeadReading, error)
}

// forgeRepository is whether a turn's repository names a forge repository (owner/name), the only place a pull
// request number can be read from the turn alone.
func forgeRepository(repository string) bool {
	return !filepath.IsAbs(repository) && slug.MatchString(repository)
}

// PullRequestHead reads one pull request with a single gh api GET and takes nothing from the answer that it does
// not name: the number it was asked for and a full lower-case object name as the head.
func (r TargetReader) PullRequestHead(ctx context.Context, repository string, number int64) (PullRequestHeadReading, error) {
	if !forgeRepository(repository) {
		return PullRequestHeadReading{}, unreadable("repository %s is not an owner/name forge repository, so there is no pull request this can read", pyvalue.StrRepr(repository))
	}
	if number < 1 {
		return PullRequestHeadReading{}, unreadable("pull request %d does not exist: a pull request number is a positive whole number", number)
	}
	gh := r.GH
	if gh == "" {
		gh = "gh"
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, ForgeCallTimeout)
	defer cancel()
	cmd := exec.CommandContext(timeoutCtx, gh, "api", "--method", "GET", "-H", "Accept: application/vnd.github+json", fmt.Sprintf("repos/%s/pulls/%d", repository, number))
	output, err := cmd.Output()
	if err != nil {
		if timeoutCtx.Err() == context.DeadlineExceeded {
			return PullRequestHeadReading{}, unreadable("reading pull request %d exceeded the timeout of %d seconds, so no answer was observed", number, int(ForgeCallTimeout/time.Second))
		}
		if e, ok := err.(*exec.ExitError); ok {
			detail := excerpt(strings.TrimSpace(string(e.Stderr)))
			if strings.Contains(detail, "HTTP 404") {
				return PullRequestHeadReading{}, unreadable("pull request %d does not exist in %s", number, repository)
			}
			return PullRequestHeadReading{}, unreadable("reading pull request %d failed: %s", number, detail)
		}
		return PullRequestHeadReading{}, unreadable("the forge could not be read: %v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.UseNumber()
	var payload any
	if decoder.Decode(&payload) != nil {
		return PullRequestHeadReading{}, unreadable("reading pull request %d returned something that is not JSON", number)
	}
	if _, extra := decoder.Token(); extra != io.EOF {
		return PullRequestHeadReading{}, unreadable("reading pull request %d returned something that is not JSON", number)
	}
	data, ok := payload.(map[string]any)
	if !ok {
		return PullRequestHeadReading{}, unreadable("the forge answered with something that is not one pull request")
	}
	// the number is compared as the integer it is: a float64 would take a neighbouring large number for the one asked
	if named, _ := data["number"].(json.Number); named.String() != strconv.FormatInt(number, 10) {
		return PullRequestHeadReading{}, unreadable("the forge's answer does not name pull request %d", number)
	}
	head, _ := data["head"].(map[string]any)
	sha, _ := head["sha"].(string)
	if !githubSHA.MatchString(sha) {
		return PullRequestHeadReading{}, unreadable("the forge's answer for pull request %d does not name a full commit as its head", number)
	}
	host := os.Getenv("GH_HOST")
	if host == "" {
		host = "github.com"
	}
	return PullRequestHeadReading{Repository: repository, Number: number, SHA: sha, Source: "github:" + host}, nil
}

// pullRequestRead is a pull request head read before a transaction, with the head it was read for. A read that failed
// keeps why.
type pullRequestRead struct {
	forHead string
	reading PullRequestHeadReading
	failed  string
}

// readPullRequest reads the pull request a turn is bound to, for the head about to be declared or restated. It runs
// outside any transaction: a forge call can take as long as ForgeCallTimeout.
func readPullRequest(ctx context.Context, pulls PullRequestHeadReader, row store.MergeTurnsRow, head string) *pullRequestRead {
	read := &pullRequestRead{forHead: head}
	reading, err := pulls.PullRequestHead(ctx, row.Repository, row.PRNumber.Int64)
	if err != nil {
		read.failed = err.Error()
		return read
	}
	read.reading = reading
	return read
}

// pullRequestVerdict decides whether head is the head of the pull request the turn is bound to, and says how: the
// refusal, or the pullRequestHead of the answer. verb is what the caller does with the head ("declares", "restates").
//
// The turn's own record decides alone when there is nothing to compare, which the caller has already checked (no
// recorded pull request, a head that did not move) and when the repository is a local path: the pull request's forge
// identity is not in the turn, so nothing can be read, and the answer says so. Otherwise the forge decides: a head
// that is not the pull request's is another candidate, and a pull request that was not read for this head decides
// nothing.
func pullRequestVerdict(row store.MergeTurnsRow, actor, verb, head string, read *pullRequestRead) (*registry.CoordinationRefusal, map[string]any) {
	number := row.PRNumber.Int64
	if !forgeRepository(row.Repository) {
		return nil, map[string]any{"pullRequest": number, "decidedBy": "record", "head": nil, "reason": "the repository " + pyvalue.StrRepr(row.Repository) + " is not an owner/name forge repository, so the pull request head was not read"}
	}
	unread := func(why string) *registry.CoordinationRefusal {
		return coordination(row, contract.RefusalMergeTargetUnreadable, fmt.Sprintf("the head of pull request %d of %s was not read, so the head this call %s for turn %s cannot be compared with it: %s", number, row.Repository, verb, pyvalue.StrRepr(row.TurnID), why), row.TurnID, actor)
	}
	switch {
	case read == nil || read.forHead != head:
		return unread("the turn changed during the call before the pull request was read for this head; call again"), nil
	case read.failed != "":
		return unread(read.failed + "; call again once the forge answers"), nil
	case !SameCommit(read.reading.SHA, head):
		return coordination(row, contract.RefusalMergeCandidateMoved, fmt.Sprintf("turn %s is bound to pull request %d of %s, which reads head %s on the forge (%s), and this call %s head %s for it. A head that is not the pull request's head is another candidate, so it was not taken. If the branch was just refreshed, read the pull request again and %s the head it shows; for another pull request, ask for its own turn after this one lands or is returned", pyvalue.StrRepr(row.TurnID), number, row.Repository, pyvalue.StrRepr(read.reading.SHA), read.reading.Source, verb, pyvalue.StrRepr(head), verbOf(verb)), read.reading.SHA, head), nil
	}
	return nil, map[string]any{"pullRequest": number, "decidedBy": "forge", "head": read.reading.SHA, "source": read.reading.Source}
}

// verbOf is the verb of the instruction that answers a verb of the statement ("declares" -> "declare").
func verbOf(verb string) string { return strings.TrimSuffix(verb, "s") }
