package pullrequest

// `crw flow pr comments` (CRW-1460): collects the known review bots' comments of a pull request (issue comments, inline
// review comments and reviews), deduplicates identical findings into items keyed by the sha256 of their normalized
// (body, path, first observed line), applies the caller's dispositions and persists the crw-pr-review/1 record. At most two rounds: a
// second round that still has unresolved items answers round_limit.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
)

// CommentDisposition is one decided item of a review round: applied or rejected, with its reason.
type CommentDisposition struct {
	Key         string `json:"key"`
	Disposition string `json:"disposition"`
	Reason      string `json:"reason"`
}

// CommentsInput is what `crw flow pr comments` is given.
type CommentsInput struct {
	// PR is the pull request as OWNER/NAME#N; Head the full commit it must still carry.
	PR, Head string
	// Record is the crw-pr-review/1 file this round reads and writes (required).
	Record string
	// Bots is connected or not_connected.
	Bots string
	// Round is 1 or 2: the same round again refreshes idempotently; a round above the record's increments it once.
	Round        int
	Dispositions []CommentDisposition
}

// noteNoBots is the note a not_connected record carries.
const noteNoBots = "리뷰 봇 미연결"

func normalizedCommentBody(body string) string {
	return strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n"))
}

// commentKey is the deduplication key of a comment: the sha256 of the JSON tuple [normalized body, path, line], where the
// body is CRLF-normalized and trimmed on the outside only.
func commentKey(c tracking.PRReviewComment) (string, error) {
	body := normalizedCommentBody(c.Body)
	tuple, err := json.Marshal([3]any{body, c.Path, c.Line})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(tuple)
	return hex.EncodeToString(sum[:]), nil
}

// Comments runs one review round over the pull request: reads it before and after the collection (the head must stay the
// pinned one), merges the collected comments into the previous record's items, applies the dispositions and writes the
// record. Input mistakes and transport failures are errors; the semantic answer is the record's state.
func Comments(ctx context.Context, source interface {
	PullRequest(context.Context, tracking.Ref) (tracking.PullRequest, error)
	ReviewComments(context.Context, tracking.Ref) ([]tracking.PRReviewComment, error)
}, in CommentsInput) (tracking.PRReviewRecord, error) {
	ref, err := tracking.ParseRef(in.PR)
	if err != nil {
		return tracking.PRReviewRecord{}, fmt.Errorf("%w: --pr %q: %v", ErrDecideInput, in.PR, err)
	}
	if !decideSHA.MatchString(in.Head) {
		return tracking.PRReviewRecord{}, fmt.Errorf("%w: --head %q is not a full commit id", ErrDecideInput, in.Head)
	}
	if strings.TrimSpace(in.Record) == "" {
		return tracking.PRReviewRecord{}, fmt.Errorf("%w: --record is required", ErrDecideInput)
	}
	if in.Bots != tracking.ReviewBotsConnected && in.Bots != tracking.ReviewBotsNotConnected {
		return tracking.PRReviewRecord{}, fmt.Errorf("%w: --bots %q is not connected or not_connected", ErrDecideInput, in.Bots)
	}
	if in.Round < 1 || in.Round > 2 {
		return tracking.PRReviewRecord{}, fmt.Errorf("%w: --round %d is not 1 or 2", ErrDecideInput, in.Round)
	}
	// the dispositions are checked before any network call: values, reasons and no duplicate keys
	seen := map[string]bool{}
	for _, dis := range in.Dispositions {
		if dis.Key == "" {
			return tracking.PRReviewRecord{}, fmt.Errorf("%w: a disposition with an empty key", ErrDecideInput)
		}
		if seen[dis.Key] {
			return tracking.PRReviewRecord{}, fmt.Errorf("%w: disposition of key %s twice", ErrDecideInput, dis.Key)
		}
		seen[dis.Key] = true
		if dis.Disposition != tracking.DispositionApplied && dis.Disposition != tracking.DispositionRejected {
			return tracking.PRReviewRecord{}, fmt.Errorf("%w: disposition %q is not applied or rejected", ErrDecideInput, dis.Disposition)
		}
		if strings.TrimSpace(dis.Reason) == "" {
			return tracking.PRReviewRecord{}, fmt.Errorf("%w: the disposition of %s has no reason", ErrDecideInput, dis.Key)
		}
	}
	if source == nil {
		return tracking.PRReviewRecord{}, errors.New("no pull request source is wired")
	}
	// one round at a time per record: flock the sibling lock file
	unlock, err := decideLock(in.Record + ".lock")
	if err != nil {
		return tracking.PRReviewRecord{}, err
	}
	defer unlock()
	// the previous record, when there is one, bounds the round and carries its items over; it is read through the same
	// bounded reader mergeRequest uses, of this pull request whatever head it is of
	var prev *tracking.PRReviewRecord
	if _, err := os.Stat(in.Record); err == nil {
		rec, err := tracking.ReadPRReviewHistory(in.Record, ref)
		if err != nil {
			return tracking.PRReviewRecord{}, fmt.Errorf("the existing record: %w", err)
		}
		prev = &rec
	} else if !errors.Is(err, os.ErrNotExist) {
		return tracking.PRReviewRecord{}, err
	}
	prevRound := 0
	if prev != nil {
		prevRound = prev.Round
	}
	if in.Round < prevRound || in.Round > prevRound+1 {
		return tracking.PRReviewRecord{}, fmt.Errorf("%w: round %d does not follow the record's round %d (the same round refreshes, the next continues)", ErrDecideInput, in.Round, prevRound)
	}
	if prev != nil && prev.Head != in.Head && in.Round != prev.Round+1 {
		return tracking.PRReviewRecord{}, fmt.Errorf("%w: the pull request's head changed since the record's round %d: a changed head needs the next review round", ErrDecideInput, prev.Round)
	}
	// the pull request must carry the pinned head, before and after the collection
	pr, err := source.PullRequest(ctx, ref)
	if err != nil {
		return tracking.PRReviewRecord{}, err
	}
	if pr.HeadSHA != in.Head {
		return tracking.PRReviewRecord{}, fmt.Errorf("%s is at %s, not the pinned head %s", ref, pr.HeadSHA, in.Head)
	}
	comments, err := source.ReviewComments(ctx, ref)
	if err != nil {
		return tracking.PRReviewRecord{}, err
	}
	again, err := source.PullRequest(ctx, ref)
	if err != nil {
		return tracking.PRReviewRecord{}, err
	}
	if again.HeadSHA != in.Head {
		return tracking.PRReviewRecord{}, fmt.Errorf("%s moved to %s during the collection", ref, again.HeadSHA)
	}
	if in.Bots == tracking.ReviewBotsNotConnected {
		// disconnected is only recorded when no known bot comment exists anywhere — neither collected now nor recorded before
		if len(comments) != 0 || (prev != nil && len(prev.Items) != 0) {
			return tracking.PRReviewRecord{}, errors.New("the pull request has review bot comments: --bots cannot be not_connected")
		}
		rec := tracking.PRReviewRecord{Schema: tracking.PRReviewSchema, PR: ref.String(), Head: in.Head, Bots: in.Bots,
			Round: in.Round, State: tracking.ReviewStateNotConnected, Note: noteNoBots, Items: []tracking.PRReviewItem{}}
		if err := writeJSONRecordFile(in.Record, rec); err != nil {
			return tracking.PRReviewRecord{}, err
		}
		return rec, nil
	}
	// merge the collection into the previous items: a comment of a known key joins the item, a new key is a new item, and a
	// source id that changed its body or path moves out of its old item (which is dropped only when nothing else observes it)
	items := []tracking.PRReviewItem{}
	byKey := map[string]int{}
	bySource := map[string]int{}
	if prev != nil {
		for _, it := range prev.Items {
			idx := len(items)
			items = append(items, it)
			byKey[it.Key] = idx
			for _, c := range it.Comments {
				bySource[c.ID] = idx
			}
		}
	}
	dropped := map[int]bool{}
	for _, c := range comments {
		key, err := commentKey(c)
		if err != nil {
			return tracking.PRReviewRecord{}, err
		}
		if old, ok := bySource[c.ID]; ok && !dropped[old] {
			for _, oc := range items[old].Comments {
				if oc.ID == c.ID && oc.Path == c.Path && normalizedCommentBody(oc.Body) == normalizedCommentBody(c.Body) {
					key = items[old].Key
					break
				}
			}
		}
		if old, ok := bySource[c.ID]; ok && !dropped[old] && items[old].Key != key {
			kept := items[old].Comments[:0]
			for _, oc := range items[old].Comments {
				if oc.ID != c.ID {
					kept = append(kept, oc)
				}
			}
			items[old].Comments = kept
			delete(bySource, c.ID)
			if len(items[old].Comments) == 0 {
				dropped[old] = true
				delete(byKey, items[old].Key)
			}
		}
		if idx, ok := byKey[key]; ok && !dropped[idx] {
			known := false
			for i, oc := range items[idx].Comments {
				if oc.ID == c.ID {
					items[idx].Comments[i] = c
					known = true
					break
				}
			}
			if !known {
				items[idx].Comments = append(items[idx].Comments, c)
			}
			bySource[c.ID] = idx
			continue
		}
		idx := len(items)
		items = append(items, tracking.PRReviewItem{Key: key, Comments: []tracking.PRReviewComment{c}})
		byKey[key] = idx
		bySource[c.ID] = idx
	}
	compacted := items[:0]
	for i, it := range items {
		if dropped[i] {
			continue
		}
		if n := len(it.Comments); n > 0 {
			compacted = append(compacted, it)
		}
	}
	items = compacted
	// every disposition names a known item (the keys re-index after compaction)
	byKey = make(map[string]int, len(items))
	for i := range items {
		byKey[items[i].Key] = i
	}
	for _, dis := range in.Dispositions {
		i, ok := byKey[dis.Key]
		if !ok {
			return tracking.PRReviewRecord{}, fmt.Errorf("%w: the disposition names key %s, which no item carries", ErrDecideInput, dis.Key)
		}
		items[i].Disposition = dis.Disposition
		items[i].Reason = dis.Reason
	}
	rec := tracking.PRReviewRecord{Schema: tracking.PRReviewSchema, PR: ref.String(), Head: in.Head, Bots: in.Bots, Round: in.Round, Items: items}
	unresolved := 0
	for _, it := range items {
		if it.Disposition == "" {
			unresolved++
		}
	}
	switch {
	case unresolved == 0:
		rec.State = tracking.ReviewStateComplete
	case in.Round == 2:
		rec.State = tracking.ReviewStateRoundLimit
	default:
		rec.State = tracking.ReviewStatePending
	}
	if err := writeJSONRecordFile(in.Record, rec); err != nil {
		return tracking.PRReviewRecord{}, err
	}
	return rec, nil
}
