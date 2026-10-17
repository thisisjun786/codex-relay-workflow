package crwconfig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// GitHubWorkflowInfo is the one workflow of a repository's actions workflows listing the remote verifier
// selects (CRW-1594): its numeric ID, name, path below .github/workflows/ and state.
type GitHubWorkflowInfo struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Path  string `json:"path"`
	State string `json:"state"`
}

// githubWorkflowListPage is one page of repos/OWNER/REPO/actions/workflows.
type githubWorkflowListPage struct {
	Workflows []GitHubWorkflowInfo `json:"workflows"`
}

var githubWorkflowFilename = regexp.MustCompile(`^\.github/workflows/[^/\x00-\x1f]+$`)

// githubSelectorDigits is a selector that is a numeric workflow ID.
var githubSelectorDigits = regexp.MustCompile(`^[0-9]+$`)

// DecodeGitHubWorkflow selects exactly one workflow from the sequential JSON objects `gh api --paginate
// repos/OWNER/REPO/actions/workflows?per_page=100` answers (one {"workflows":[...]} object per page, decoded
// until EOF). The selector matches the configured name, a numeric ID, the full workflow path or the
// filename; an absent or ambiguous match, an ID that is not positive, a state that is not active and a path
// that is not a single filename below .github/workflows/ are all errors.
func DecodeGitHubWorkflow(raw []byte, selector string) (GitHubWorkflowInfo, error) {
	var workflows []GitHubWorkflowInfo
	decoder := json.NewDecoder(bytes.NewReader(raw))
	for {
		var page githubWorkflowListPage
		err := decoder.Decode(&page)
		if err == io.EOF {
			break
		}
		if err != nil {
			return GitHubWorkflowInfo{}, fmt.Errorf("the workflows listing could not be read: %v", err)
		}
		workflows = append(workflows, page.Workflows...)
	}
	matches := func(w GitHubWorkflowInfo) bool {
		if githubSelectorDigits.MatchString(selector) {
			return strconv.FormatInt(w.ID, 10) == selector
		}
		if w.Name == selector || w.Path == selector {
			return true
		}
		if i := strings.LastIndex(w.Path, "/"); i >= 0 && w.Path[i+1:] == selector {
			return true
		}
		return false
	}
	var selected *GitHubWorkflowInfo
	for i := range workflows {
		if matches(workflows[i]) {
			if selected != nil {
				return GitHubWorkflowInfo{}, fmt.Errorf("the workflow selector %q matches more than one workflow", selector)
			}
			selected = &workflows[i]
		}
	}
	if selected == nil {
		return GitHubWorkflowInfo{}, fmt.Errorf("the workflow selector %q matches no workflow", selector)
	}
	if selected.ID <= 0 {
		return GitHubWorkflowInfo{}, fmt.Errorf("the workflow %q has no positive id", selector)
	}
	if selected.State != "active" {
		return GitHubWorkflowInfo{}, fmt.Errorf("the workflow %q is %s, not active", selector, selected.State)
	}
	if !githubWorkflowFilename.MatchString(selected.Path) || strings.Contains(selected.Path, "..") {
		return GitHubWorkflowInfo{}, fmt.Errorf("the workflow %q is %s, not a single file below .github/workflows/", selector, selected.Path)
	}
	return *selected, nil
}
