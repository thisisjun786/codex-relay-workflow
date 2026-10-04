package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/quote"
	"github.com/thisisjun786/codex-relay-workflow/internal/review"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
)

// A child that ran the independent code review states what it did with the result in an
// independentReview item: the artifact the review command wrote (its absolute path and the sha256
// of its bytes), the status and reason, how many reviewer calls were unusable, and one disposition
// for each finding it answered. The review is a reference opinion for the parent and never a merge
// gate, so everything this file finds is a warning: a ReviewCoverage is read beside the
// restatement, never added to its problems, and has no way to change a verdict or an exit code.

// IndependentReviewMember is the member of a handoff record, and of a receipt, that carries the
// child's statement about its independent code review.
const IndependentReviewMember = "independentReview"

// The warning codes.
const (
	ReviewAbsent             = "independent_review_absent"
	ReviewMalformed          = "independent_review_malformed"
	ReviewUnreadable         = "independent_review_unreadable"
	ReviewSHA256Mismatch     = "independent_review_sha256_mismatch"
	ReviewArtifactInvalid    = "independent_review_artifact_invalid"
	ReviewStatusDiffers      = "independent_review_status_differs"
	ReviewHeadDiffers        = "independent_review_head_differs"
	ReviewDispositionMissing = "independent_review_disposition_missing"
	ReviewDispositionUnknown = "independent_review_disposition_unknown"
)

// reviewArtifactCap is the most bytes of an artifact file that are read. It is a variable only so
// that a test can lower it.
var reviewArtifactCap int64 = 8 << 20

var (
	errArtifactTooLarge = errors.New("larger than the cap")

	reviewStatuses     = []string{"complete", "partial", "unavailable"}
	reviewDispositions = []string{"fixed", "refuted", "recorded"}
	itemMembers        = []string{"artifact", "status", "reason", "invalidReviewerCalls", "headPatchId", "dispositions"}
	digest64           = regexp.MustCompile(`^[0-9a-f]{64}$`)
	commitDigest       = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
)

// ArtifactReader reads the artifact file an item names; nil reads the file itself, bounded.
type ArtifactReader func(path string) ([]byte, error)

// ReviewCoverage is what reading one record's independentReview item says. Stated is whether the
// record carries the item at all.
type ReviewCoverage struct {
	Stated   bool
	Warnings []Problem
}

type reviewItem struct {
	path, sha256, status, headPatchID string
	hasArtifact                       bool
	dispositions                      []int // the findings that have an entry, in the order stated
}

// IndependentReviewCoverage reads the item of a handoff record against the artifact it names and
// the head the record is about. head is the candidate's head. An item that is not well formed is
// all it says; a file that cannot be read, or whose bytes are not the stated sha256, ends the
// comparison with that one warning, because nothing read from other bytes would describe the file the
// child meant. What the file holds, a validator's message and an operating-system error are never
// repeated in a warning.
func IndependentReviewCoverage(head string, record any, read ArtifactReader) ReviewCoverage {
	var value any
	if o, isObject := Object(record); isObject {
		value = o.Get(IndependentReviewMember)
	}
	if value == nil {
		return ReviewCoverage{Warnings: []Problem{{Code: ReviewAbsent, Detail: "the record states no " + IndependentReviewMember + " item, so nothing says whether the independent review ran or what became of its findings; the review is a reference opinion and its absence blocks nothing"}}}
	}
	coverage := ReviewCoverage{Stated: true}
	warn := func(code, detail string) {
		coverage.Warnings = append(coverage.Warnings, Problem{Code: code, Detail: detail})
	}
	item, malformed := readReviewItem(value)
	if len(malformed) > 0 {
		for _, detail := range malformed {
			warn(ReviewMalformed, "the "+IndependentReviewMember+" item is malformed: "+detail)
		}
		return coverage
	}
	if !item.hasArtifact {
		return coverage // unavailable, and no artifact was written: there is nothing to compare
	}
	if read == nil {
		read = readArtifactFile
	}
	data, err := read(item.path)
	if err != nil {
		warn(ReviewUnreadable, "the review artifact cannot be read ("+unreadableReason(err)+"), so the item was not compared with it")
		return coverage
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != item.sha256 {
		warn(ReviewSHA256Mismatch, "the review artifact's bytes hash to "+hex.EncodeToString(sum[:])+", not the "+item.sha256+" the item states, so the file is not the one the child described and the item was not compared with it")
		return coverage
	}
	var named struct {
		Head string `json:"head"`
	}
	_ = json.Unmarshal(data, &named) // a file that is not JSON leaves the head empty, which ParseArtifact refuses
	artifact, err := review.ParseArtifact(data, named.Head)
	if err != nil {
		problems := 1
		if refusal, isList := err.(review.ValidationError); isList {
			problems = len(refusal)
		}
		warn(ReviewArtifactInvalid, "the review artifact is not a valid schema "+review.SchemaV1+" artifact ("+strconv.Itoa(problems)+" problems), so the item was not compared with it")
		return coverage
	}
	if string(artifact.Status) != item.status {
		warn(ReviewStatusDiffers, "the item states status "+quote.Value(item.status)+" and the artifact says "+quote.Value(string(artifact.Status)))
	}
	if artifact.Head != head {
		switch {
		case item.headPatchID == "":
			warn(ReviewHeadDiffers, "the review covered head "+artifact.Head+", not "+head+", and the item states no headPatchId for it, so a base refresh cannot be told from changed code")
		case item.headPatchID != artifact.PatchID:
			warn(ReviewHeadDiffers, "the review covered head "+artifact.Head+" with patch-id "+artifact.PatchID+" and the item states patch-id "+item.headPatchID+" for "+head+", so the code changed after the review")
		}
	}
	for i, found := range artifact.Findings {
		if (found.Grade == review.P0 || found.Grade == review.P1 || found.Security) && !slices.Contains(item.dispositions, i) {
			grade := string(found.Grade)
			if found.Security {
				grade += ", security"
			}
			warn(ReviewDispositionMissing, "finding "+strconv.Itoa(i)+" ("+grade+") of the artifact has no disposition: fix it, refute it with code evidence or record it")
		}
	}
	for _, i := range item.dispositions {
		if i >= len(artifact.Findings) {
			warn(ReviewDispositionUnknown, "the item states a disposition for finding "+strconv.Itoa(i)+", and the artifact has "+strconv.Itoa(len(artifact.Findings))+" findings")
		}
	}
	return coverage
}

// readReviewItem reads the item as the contract states it, one detail per departure.
func readReviewItem(value any) (reviewItem, []string) {
	var item reviewItem
	var bad []string
	o, isObject := Object(value)
	if !isObject {
		return item, []string{"it is an object, not " + quote.Kind(value)}
	}
	for _, field := range o {
		if !slices.Contains(itemMembers, field.Key) {
			bad = append(bad, "it has no member "+quote.Value(field.Key))
		}
	}
	str := func(from func(string) (any, bool), name string) (string, bool) {
		raw, present := from(name)
		text, isText := raw.(string)
		if present && !isText {
			bad = append(bad, name+" is a string, not "+quote.Kind(raw))
		}
		return text, isText
	}
	status, _ := str(o.Lookup, "status")
	if !slices.Contains(reviewStatuses, status) {
		bad = append(bad, "status is one of "+strings.Join(reviewStatuses, ", ")+", not "+quote.Value(o.Get("status")))
	}
	item.status = status
	if reason, _ := str(o.Lookup, "reason"); status != "complete" && slices.Contains(reviewStatuses, status) && strings.TrimSpace(reason) == "" {
		bad = append(bad, "a status other than complete states its reason")
	}
	if n, isWhole := Whole(o.Get("invalidReviewerCalls")); !isWhole || n.Sign() < 0 {
		bad = append(bad, "invalidReviewerCalls is a count of reviewer calls, not "+quote.Value(o.Get("invalidReviewerCalls")))
	}
	if id, given := str(o.Lookup, "headPatchId"); given {
		if !commitDigest.MatchString(id) {
			bad = append(bad, "headPatchId is 40 or 64 lowercase hex digits, not "+quote.Value(id))
		}
		item.headPatchID = id
	}
	if artifact, given := o.Lookup("artifact"); given {
		item.hasArtifact = true
		a, isObject := Object(artifact)
		if !isObject {
			bad = append(bad, "artifact is an object stating path and sha256, not "+quote.Kind(artifact))
		}
		for _, field := range a {
			if field.Key != "path" && field.Key != "sha256" {
				bad = append(bad, "artifact has no member "+quote.Value(field.Key))
			}
		}
		path, _ := str(a.Lookup, "path")
		sum, _ := str(a.Lookup, "sha256")
		if isObject && !strings.HasPrefix(path, "/") {
			bad = append(bad, "artifact.path is an absolute path, not "+quote.Value(path))
		}
		if isObject && !digest64.MatchString(sum) {
			bad = append(bad, "artifact.sha256 is 64 lowercase hex digits, not "+quote.Value(sum))
		}
		item.path, item.sha256 = path, sum
	} else if status != "unavailable" {
		bad = append(bad, "a review that produced no artifact has the status unavailable; a "+quote.Value(status)+" one names its artifact")
	}
	listed, isList := List(o.Get("dispositions"))
	if !isList {
		return item, append(bad, "dispositions is a list, not "+quote.Kind(o.Get("dispositions")))
	}
	for position, raw := range listed {
		where := "dispositions[" + strconv.Itoa(position) + "] "
		entry, isEntry := Object(raw)
		if !isEntry {
			bad = append(bad, where+"is an object, not "+quote.Kind(raw))
			continue
		}
		for _, field := range entry {
			if !slices.Contains([]string{"finding", "disposition", "evidence"}, field.Key) {
				bad = append(bad, where+"has no member "+quote.Value(field.Key))
			}
		}
		n, isWhole := Whole(entry.Get("finding"))
		if !isWhole || n.Sign() < 0 || !n.IsInt64() {
			bad = append(bad, where+"names a finding by its position in the artifact, not "+quote.Value(entry.Get("finding")))
			continue
		}
		if kind, _ := str(entry.Lookup, "disposition"); !slices.Contains(reviewDispositions, kind) {
			bad = append(bad, where+"disposition is one of "+strings.Join(reviewDispositions, ", ")+", not "+quote.Value(entry.Get("disposition")))
		}
		if evidence, _ := str(entry.Lookup, "evidence"); strings.TrimSpace(evidence) == "" {
			bad = append(bad, where+"states its evidence")
		}
		if slices.Contains(item.dispositions, int(n.Int64())) {
			bad = append(bad, where+"repeats the finding of an earlier entry")
			continue
		}
		item.dispositions = append(item.dispositions, int(n.Int64()))
	}
	return item, bad
}

// unreadableReason is one of four fixed phrases; the error itself can hold paths and system text.
func unreadableReason(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "the path does not exist"
	case errors.Is(err, syscall.EINVAL): // what reading.OpenRegular says of a file that is not regular
		return "it is not a regular file"
	case errors.Is(err, errArtifactTooLarge):
		return "it is larger than " + strconv.FormatInt(reviewArtifactCap, 10) + " bytes"
	}
	return "it cannot be read"
}

// readArtifactFile reads a regular file of at most reviewArtifactCap bytes and nothing else.
func readArtifactFile(path string) ([]byte, error) {
	file, err := reading.OpenRegular(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, reviewArtifactCap+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > reviewArtifactCap {
		return nil, errArtifactTooLarge
	}
	return data, nil
}
