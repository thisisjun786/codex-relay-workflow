package adjudication

import (
	"crypto/sha256"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/review"
)

// ReadArtifact imports bytes the parent already has, after verifying their identity.
func ReadArtifact(data []byte, pr PR, expectedSHA256 string) (Run, error) {
	if err := pr.validate(); err != nil {
		return Run{}, err
	}
	if len(data) > maxRecordBytes || !hash.MatchString(expectedSHA256) || fmt.Sprintf("%x", sha256.Sum256(data)) != expectedSHA256 {
		return Run{}, fmt.Errorf("artifact size or sha256 mismatch")
	}
	a, err := review.ParseArtifact(data, pr.Head)
	if err != nil {
		return Run{}, err
	}
	r := Run{ID: fmt.Sprintf("%s#%d:%s:%s", pr.Repository, pr.Number, pr.Head, expectedSHA256), PR: pr, Reviewer: Reviewer{Independent, a.Model}, ArtifactSHA256: expectedSHA256, Status: string(a.Status), Reason: a.Reason, Reviewers: &a.Reviewers.Run, Findings: []Finding{}}
	if a.Calls != nil {
		r.Calls = []Call{}
	}
	for _, c := range a.Calls {
		r.Calls = append(r.Calls, Call{Record: c, ElapsedKnown: true, TokensKnown: false})
	}
	for i, f := range a.Findings {
		r.Findings = append(r.Findings, Finding{fmt.Sprint(i), f.File, f.Line, f.Title, f.Grade, f.Security})
	}
	return r, r.validate()
}

// ReadExport reads normalized saved Devin/Codex Run JSON. It fetches nothing and trusts no reviewer verdict.
func ReadExport(data []byte, expectedPR PR) (Run, error) {
	var r Run
	if err := decode(data, &r); err != nil {
		return r, err
	}
	if r.Source != Devin && r.Source != Codex || r.PR != expectedPR {
		return r, fmt.Errorf("export source or PR identity mismatch")
	}
	return r, r.validate()
}
