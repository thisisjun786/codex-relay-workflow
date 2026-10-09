package command

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/review"
)

// prComment is a general comment of a pull request (an issue comment of the forge, not a comment of a review).
type prComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	URL  string `json:"html_url"`
}

// forge is the access to a pull request that --post-summary needs. It reaches general comments only: it has no way to start or reply to a review thread, because a thread that arrives after the child's
// receipt would void the handoff.
type forge interface {
	List(ctx context.Context, pr int) ([]prComment, error)
	Create(ctx context.Context, pr int, body string) (prComment, error)
	Update(ctx context.Context, id int64, body string) (prComment, error)
}

// forgeFor is the forge --post-summary talks to: the one the environment supplies (tests), else the gh CLI run in the checkout, which resolves {owner}/{repo} from the checkout's remotes (or GH_REPO).
func (e env) forgeFor(cfg Config) forge {
	if e.forge != nil {
		if f := e.forge(cfg); f != nil {
			return f
		}
	}
	return ghForge{binary: cfg.Gh, dir: cfg.Repo}
}

// ghForge is forge over "gh api". The body travels on stdin (-F body=@-), so no text of a review reaches a command line.
type ghForge struct{ binary, dir string }

func (g ghForge) api(ctx context.Context, body string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if body != "" {
		args = append(args, "-F", "body=@-")
	}
	c := exec.CommandContext(ctx, g.binary, append([]string{"api"}, args...)...)
	c.Dir, c.WaitDelay = g.dir, 5*time.Second
	if body != "" {
		c.Stdin = strings.NewReader(body)
	}
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	if err := c.Run(); err != nil {
		line, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n")
		return nil, fmt.Errorf("gh api %s: %w: %s", strings.Join(args, " "), err, line)
	}
	return stdout.Bytes(), nil
}

// List returns the comments, oldest first. gh prints the pages one after another, so the output is read as a sequence of arrays.
func (g ghForge) List(ctx context.Context, pr int) ([]prComment, error) {
	out, err := g.api(ctx, "", "--method", "GET", "--paginate", fmt.Sprintf("repos/{owner}/{repo}/issues/%d/comments?per_page=100", pr))
	if err != nil {
		return nil, err
	}
	var all []prComment
	for dec := json.NewDecoder(bytes.NewReader(out)); ; {
		var page []prComment
		if err := dec.Decode(&page); err == io.EOF {
			return all, nil
		} else if err != nil {
			return nil, fmt.Errorf("the comments of pull request %d are not JSON: %w", pr, err)
		}
		all = append(all, page...)
	}
}

func (g ghForge) one(ctx context.Context, body string, args ...string) (c prComment, err error) {
	out, err := g.api(ctx, body, args...)
	if err == nil && (json.Unmarshal(out, &c) != nil || c.ID == 0) {
		err = fmt.Errorf("gh api %s did not answer with a comment", strings.Join(args, " "))
	}
	return c, err
}

func (g ghForge) Create(ctx context.Context, pr int, body string) (prComment, error) {
	return g.one(ctx, body, "--method", "POST", fmt.Sprintf("repos/{owner}/{repo}/issues/%d/comments", pr))
}

func (g ghForge) Update(ctx context.Context, id int64, body string) (prComment, error) {
	return g.one(ctx, body, "--method", "PATCH", fmt.Sprintf("repos/{owner}/{repo}/issues/comments/%d", id))
}

// assignedTo reports whether the ledger assigned the result with this sha256 to path: a file that has these bytes is the result of some patch, only not the one that is posted.
func assignedTo(recs []record, path, sha string) bool {
	for _, r := range recs {
		if r.Artifact == path && r.SHA256 == sha && (r.Event == "finished" || r.Event == "unavailable") {
			return true
		}
	}
	return false
}

// postSummary keeps the one summary comment of pull request cfg.PR: it summarizes the newest result of the patch (its artifact's bytes must be the recorded ones, or, when the path holds the result the ledger assigned to it for another patch, the copy kept with the record), then updates the first comment whose body begins with the marker,
// creates one, or leaves it when the text is the same. The post lock is held from the reading of the ledger to the write, so two posts of one state directory cannot both find no comment and both create one, and a post that was
// prepared from an older result of the patch than the ledger holds when it gets the lock summarizes the newer one instead (sum.Comment.Reason says so) and never puts the older over it.
func postSummary(ctx context.Context, cfg Config, f forge, sum *Summary, stderr io.Writer) error {
	l := &ledger{dir: cfg.StateDir}
	unlock, err := l.postLock(ctx, cfg.LockWait)
	if err != nil {
		return err
	}
	defer unlock()
	path, sha, head, newer := sum.Artifact, sum.SHA256, sum.Head, ""
	if sum.ReviewedHead != "" {
		head = sum.ReviewedHead
	}
	recs, err := l.read()
	if err != nil {
		return err
	}
	if n := newestResult(recs, sum.PatchID); n != nil && n.SHA256 != sha {
		path, sha, head, newer = n.Artifact, n.SHA256, n.Head, fmt.Sprintf("a newer result of this patch (sha256 %s) was recorded; the comment shows it", short(n.SHA256))
	}
	data, err := os.ReadFile(path)
	if err == nil {
		digest := sha256.Sum256(data)
		if got := hex.EncodeToString(digest[:]); got != sha {
			if !assignedTo(recs, path, got) {
				return fmt.Errorf("the artifact %s has sha256 %s, not the recorded %s", path, got, sha)
			}
			err = fs.ErrNotExist // the path holds another patch's result, which the ledger assigned to it: the patch's own bytes are the kept copy
		}
	}
	if errors.Is(err, fs.ErrNotExist) { // a result whose record was appended a moment ago and whose files are still being published, or one that another patch's result took the path from: the copy kept before the record has the same bytes
		if kept, keptErr := os.ReadFile(l.keptPath(sha)); keptErr == nil {
			data, err = kept, nil
		}
	}
	if err != nil {
		return fmt.Errorf("the artifact of the review cannot be read: %w", err)
	}
	digest := sha256.Sum256(data)
	if got := hex.EncodeToString(digest[:]); got != sha {
		return fmt.Errorf("the artifact %s has sha256 %s, not the recorded %s", path, got, sha)
	}
	a, err := review.ParseArtifact(data, head)
	if err != nil {
		return fmt.Errorf("the artifact %s is not a valid review: %w", path, err)
	}
	body := renderSummary(a, sha)
	comments, err := f.List(ctx, cfg.PR)
	if err != nil {
		return err
	}
	var marked []prComment
	for _, c := range comments {
		if strings.HasPrefix(c.Body, markerPrefix) {
			marked = append(marked, c)
		}
	}
	var c prComment
	action := "created"
	switch {
	case len(marked) == 0:
		c, err = f.Create(ctx, cfg.PR, body)
	case marked[0].Body == body:
		c, action = marked[0], "unchanged"
	default:
		c, err = f.Update(ctx, marked[0].ID, body)
		action = "updated"
	}
	if err != nil {
		return err
	}
	if len(marked) > 1 {
		fmt.Fprintf(stderr, "crw review: warning: %d comments of pull request %d carry the marker; the first was kept up to date and the others were left alone\n", len(marked), cfg.PR)
	}
	sum.Comment = &Posted{Action: action, URL: c.URL, Reason: newer}
	return nil
}
