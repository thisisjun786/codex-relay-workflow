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
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
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

// pullRequestRead is a pull request head read before a transaction, with the head it was read for and the candidate head
// the turn had when it was read. A read that failed keeps why.
type pullRequestRead struct {
	forHead   string
	candidate string
	reading   PullRequestHeadReading
	failed    string
}

// readPullRequest reads the pull request a turn is bound to, for the head about to be declared or restated. It runs
// outside any transaction: a forge call can take as long as ForgeCallTimeout.
func readPullRequest(ctx context.Context, pulls PullRequestHeadReader, row store.MergeTurnsRow, head string) *pullRequestRead {
	read := &pullRequestRead{forHead: head, candidate: row.CandidateHead}
	reading, err := pulls.PullRequestHead(ctx, row.Repository, row.PRNumber.Int64)
	if err != nil {
		read.failed = err.Error()
		return read
	}
	read.reading = reading
	return read
}

// pullRequestVerdict is the forge half of headCompareVerdict: whether head is the head of the pull request the turn is
// bound to on its forge repository, and how that was decided - the refusal, or the pullRequestHead of the answer. verb is
// what the caller does with the head ("declares", "restates"). A head that is not the pull request's is another
// candidate, and a pull request that was not read for this head of this candidate decides nothing: the turn changed
// between the read and the transaction.
func pullRequestVerdict(row store.MergeTurnsRow, actor, verb, head string, read *pullRequestRead) (*registry.CoordinationRefusal, map[string]any) {
	number := row.PRNumber.Int64
	unread := func(why string) *registry.CoordinationRefusal {
		return coordination(row, contract.RefusalMergeTargetUnreadable, fmt.Sprintf("the head of pull request %d of %s was not read, so the head this call %s for turn %s cannot be compared with it: %s", number, row.Repository, verb, pyvalue.StrRepr(row.TurnID), why), row.TurnID, actor)
	}
	switch {
	case !forgeRepository(row.Repository):
		return headCompareNothing(row, actor, verb, fmt.Sprintf("pull request %d is recorded on %s, which is not an owner/name forge repository", number, pyvalue.StrRepr(row.Repository))), nil
	case read == nil || read.forHead != head || read.candidate != row.CandidateHead:
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

// CRW-586. A head declared or restated on a turn is compared with a source of truth whenever the turn records a pull
// request or a relationship; nothing a turn records says by itself which head belongs to which pull request. The source is
// the pull request on a forge repository (owner/name), and otherwise the current work reports of the turn's relationship.
// When there is neither, the call is refused instead of passed, so no path reaches merging with a head nothing vouches for.
// A claim that records neither a pull request nor a relationship records nothing to compare, and keeps passing.
// CRW-608. A recorded forge pull request is compared with the forge only: a relay that has no pull request reader refuses the
// call too, and neither the turn's record nor a work report stands in for the reader that is missing.

// headCompareRecorded is whether the turn records a pull request or a relationship, which a head can be compared with.
func headCompareRecorded(row store.MergeTurnsRow) bool {
	return row.PRNumber.Valid || headCompareRelationship(row) != ""
}

func headCompareRelationship(row store.MergeTurnsRow) string {
	if row.RelationshipID.Valid {
		return row.RelationshipID.String
	}
	return ""
}

// headCompareForge is whether the turn's head is decided by the forge: a pull request is recorded on a forge repository.
func headCompareForge(row store.MergeTurnsRow) bool {
	return row.PRNumber.Valid && forgeRepository(row.Repository)
}

// headCompareNothing is the refusal of a call whose head has nothing to be compared with. The reason is the one for a read
// that found nothing (merge_target_unreadable), the way out is in the text, and the turn is left as it was.
func headCompareNothing(row store.MergeTurnsRow, actor, verb, why string) *registry.CoordinationRefusal {
	return coordination(row, contract.RefusalMergeTargetUnreadable, fmt.Sprintf("the head this call %s for turn %s cannot be compared with anything: %s. A head is compared with the pull request of a forge repository (owner/name) or with the work report of the assignment the turn names; claim with --pr on a forge repository, or with --relationship and retry after a work report records the head", verb, pyvalue.StrRepr(row.TurnID), why), row.TurnID, actor)
}

// headCompareVerdict decides whether head is the head of what the turn is bound to, and says how: the refusal, or the
// pullRequestHead of the answer ("forge" or "work_report"). pulls is the reader of pull requests the relay has, nil when it
// has none. Both Ready and Check call it, with the transaction's context, once the turn is the caller's to act on.
//
// A forge pull request is decided by the forge, and only by the forge. With no pull request reader (a Service built without
// Pulls; a merge-turn-check given a Reader that cannot read pull requests, whatever Service.Pulls is) the call is refused
// merge_target_unreadable: the turn's record never decides it and a work report of its relationship never stands in for the
// reader. The Service does not build a reader of its own; the relay commands supply one. Every other turn that records a pull
// request or a relationship is decided by the work report.
func (s *Service) headCompareVerdict(ctx context.Context, row store.MergeTurnsRow, actor, verb, head string, read *pullRequestRead, pulls PullRequestHeadReader) (*registry.CoordinationRefusal, map[string]any, error) {
	switch {
	case !headCompareRecorded(row):
		return nil, nil, nil
	case headCompareForge(row):
		if pulls == nil {
			return headCompareNothing(row, actor, verb, fmt.Sprintf("this service has no pull request head reader for recorded pull request %d of %s; supply a pull request head reader and call again", row.PRNumber.Int64, row.Repository)), nil, nil
		}
		refusal, decided := pullRequestVerdict(row, actor, verb, head, read)
		return refusal, decided, nil
	case headCompareRelationship(row) != "":
		return s.headCompareWorkReportVerdict(ctx, row, actor, verb, head)
	}
	return headCompareNothing(row, actor, verb, fmt.Sprintf("pull request %d is recorded on %s, which is not an owner/name forge repository, so its head cannot be read from the forge, and the turn records no relationship whose work report could name it", row.PRNumber.Int64, pyvalue.StrRepr(row.Repository))), nil, nil
}

// headCompareWorkReportVerdict compares head with the head the current work reports of the turn's relationship name,
// inside the caller's transaction. A relationship of another project, a relationship with no work report that names a head,
// two different heads and a head that is not the reported one are each refused, with a reason that is already registered.
func (s *Service) headCompareWorkReportVerdict(ctx context.Context, row store.MergeTurnsRow, actor, verb, head string) (*registry.CoordinationRefusal, map[string]any, error) {
	rid := headCompareRelationship(row)
	if refusal, err := s.headCompareScope(ctx, row, actor); refusal != nil || err != nil {
		return refusal, nil, err
	}
	reports, err := evidence.CurrentReports(ctx, s.Store, rid)
	if err != nil {
		return nil, nil, err
	}
	heads := headCompareDistinctHeads(reports)
	if len(heads) == 0 {
		return headCompareNothing(row, actor, verb, "relationship "+pyvalue.StrRepr(rid)+" has no work report that names a head"), nil, nil
	}
	if refusal := headCompareReportRefusal(row, actor, verb, head, heads); refusal != nil {
		return refusal, nil, nil
	}
	if refusal := headCompareReportIdentityRefusal(row, actor, verb, head, reports); refusal != nil {
		return refusal, nil, nil
	}
	var number any
	if row.PRNumber.Valid {
		number = row.PRNumber.Int64
	}
	return nil, map[string]any{"pullRequest": number, "decidedBy": "work_report", "head": heads[0], "relationship": rid}, nil
}

// headCompareScope refuses a relationship that is attached to another project than the turn's: its work reports are not this
// project's to compare a head with. A relationship that is attached to no project has no scope to contradict.
func (s *Service) headCompareScope(ctx context.Context, row store.MergeTurnsRow, actor string) (*registry.CoordinationRefusal, error) {
	rid := headCompareRelationship(row)
	attachment, err := s.Registry.Attachment(ctx, rid)
	if err != nil {
		return nil, err
	}
	if o, ok := attachment.(contract.OrderedObject); ok {
		if project := o.Get("projectKey"); project != nil && project != row.ProjectKey {
			return &registry.CoordinationRefusal{Reason: contract.RefusalForeignScope, Detail: "relationship " + pyvalue.StrRepr(rid) + " belongs to project " + pyvalue.Repr(project) + ", not " + pyvalue.StrRepr(row.ProjectKey), Domain: registry.DomainMergeTarget, Subject: row.TargetKey, Challenger: actor}, nil
		}
	}
	return nil, nil
}

// headCompareDistinctHeads is the distinct heads the current work reports of a relationship name, each lower-cased and
// trimmed as SameCommit reads them, in order. Reports that name no head name nothing.
func headCompareDistinctHeads(reports []evidence.CurrentReport) []string {
	heads := make([]string, 0, len(reports))
	for _, report := range reports {
		if head := strings.ToLower(strings.TrimSpace(report.Head)); head != "" {
			heads = append(heads, head)
		}
	}
	return slices.Compact(slices.Sorted(slices.Values(heads)))
}

// headCompareReportRefusal is the refusal of a head against the heads a relationship's current work reports name: two
// different heads cannot say which one the candidate is, and one that is not the candidate is another candidate.
func headCompareReportRefusal(row store.MergeTurnsRow, actor, verb, head string, heads []string) *registry.CoordinationRefusal {
	rid := headCompareRelationship(row)
	if len(heads) > 1 {
		return &registry.CoordinationRefusal{Reason: contract.RefusalRevisionAmbiguous, Detail: "relationship " + pyvalue.StrRepr(rid) + " has work reports naming " + pyvalue.Repr(heads) + "; which one this candidate is cannot be read off them", Domain: registry.DomainMergeTarget, Subject: row.TargetKey, Incumbent: heads[0], Challenger: head}
	}
	if !SameCommit(heads[0], head) {
		return &registry.CoordinationRefusal{Reason: contract.RefusalMergeCandidateMoved, Detail: "the work report for " + pyvalue.StrRepr(rid) + " names head " + pyvalue.StrRepr(heads[0]) + " and this " + verb + " " + pyvalue.StrRepr(head), Domain: registry.DomainMergeTarget, Subject: row.TargetKey, Incumbent: heads[0], Challenger: head}
	}
	return nil
}

// headCompareReportIdentityRefusal refuses a head that the work report naming it names for another pull request, or for
// another repository when the turn is on a forge repository: the report is the relationship's, not the turn's pull request's,
// so a matching head alone does not say the report is about the pull request the turn is bound to. A turn on a local path
// cannot be matched to a repository, and a report that names no pull request has no number to contradict.
func headCompareReportIdentityRefusal(row store.MergeTurnsRow, actor, verb, head string, reports []evidence.CurrentReport) *registry.CoordinationRefusal {
	for _, report := range reports {
		if !SameCommit(report.Head, head) {
			continue
		}
		var other string
		switch {
		case row.PRNumber.Valid && report.PRNumber.Valid && report.PRNumber.Int64 != row.PRNumber.Int64:
			other = fmt.Sprintf("pull request %d, and turn %s is bound to pull request %d", report.PRNumber.Int64, pyvalue.StrRepr(row.TurnID), row.PRNumber.Int64)
		case forgeRepository(row.Repository) && report.Repository != "" && !strings.EqualFold(report.Repository, row.Repository):
			other = "repository " + pyvalue.StrRepr(report.Repository) + ", and turn " + pyvalue.StrRepr(row.TurnID) + " is bound to " + pyvalue.StrRepr(row.Repository)
		default:
			continue
		}
		return &registry.CoordinationRefusal{Reason: contract.RefusalMergeCandidateMoved, Detail: "the work report for " + pyvalue.StrRepr(headCompareRelationship(row)) + " names head " + pyvalue.StrRepr(strings.ToLower(strings.TrimSpace(report.Head))) + " for " + other + ", so the head this call " + verb + " is another pull request's; ask for the turn of the pull request the report names, or " + verbOf(verb) + " the head of the pull request this turn is bound to", Domain: registry.DomainMergeTarget, Subject: row.TargetKey, Incumbent: report.Head, Challenger: head}
	}
	return nil
}
