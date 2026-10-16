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

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/publication"
	"github.com/thisisjun786/codex-relay-workflow/internal/publishpolicy"
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
type ghForge struct{ binary, dir, repo string }

// resolve pins the repository gh itself posts to — its default target, which may differ from the
// checkout's origin remote (CRW-1549: the policy that scrubs the text must be the policy of the
// repository that receives it). A failure answers an error and nothing is posted.
func (g ghForge) resolve(ctx context.Context) (ghForge, error) {
	if g.repo != "" {
		return g, nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	c := exec.CommandContext(ctx, g.binary, "repo", "view", "--json", "nameWithOwner", "--jq", ".nameWithOwner")
	c.Dir, c.WaitDelay = g.dir, 5*time.Second
	out, err := c.Output()
	if err != nil {
		return ghForge{}, fmt.Errorf("resolve GitHub publication repository: %w", err)
	}
	repo := strings.TrimSpace(string(out))
	if err := crwconfig.ValidateRepo(repo); err != nil {
		return ghForge{}, errors.New("gh did not resolve an OWNER/NAME publication repository")
	}
	g.repo = repo
	return g, nil
}

func (g ghForge) api(ctx context.Context, body string, args ...string) ([]byte, error) {
	bound, err := g.resolve(ctx)
	if err != nil {
		return nil, err
	}
	g = bound
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if body != "" {
		args = append(args, "-F", "body=@-")
	}
	for i, a := range args {
		args[i] = strings.Replace(a, "{owner}/{repo}", g.repo, 1)
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
	if g, err := g.resolve(ctx); err != nil {
		return nil, err
	} else {
		return g.list(ctx, pr)
	}
}

func (g ghForge) list(ctx context.Context, pr int) ([]prComment, error) {
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

// prepareBody scrubs the comment body with the checkout's publication policy (CRW-1549): postSummary
// already does this, so a prepared copy that reaches here again changes nothing; a direct call gets
// the same treatment. The event, if the body changed or was stopped, goes to stderr. A blocked text
// keeps the historical refusal wording and never the value.
func (g ghForge) prepareBody(ctx context.Context, body string) (string, error) {
	pol, err := publication.Policy(g.repo, os.Getenv)
	if err != nil {
		return "", err
	}
	c := publishpolicy.HostContext()
	if g.dir != "" {
		c.Paths = append(c.Paths, g.dir)
	}
	out, err := publication.Prepare(g.repo, "pr_comment", "body", body, c, pol, os.Stderr)
	if err != nil {
		return "", secretRefusal(body, err)
	}
	return out, nil
}

// secretRefusal is ErrSecret in the wording this command has always used, with the line the secret
// is on when the line-based detector sees it — the value itself is never in it.
func secretRefusal(body string, err error) error {
	if !errors.Is(err, publishpolicy.ErrSecret) {
		return err
	}
	if line, _ := publishpolicy.RawSecretLine(body); line > 0 {
		return fmt.Errorf("GitHub summary blocked (secret-in-github-text) at body:%d", line)
	}
	return errors.New("GitHub summary blocked (secret-in-github-text)")
}

func (g ghForge) Create(ctx context.Context, pr int, body string) (prComment, error) {
	g2, err := g.resolve(ctx)
	if err != nil {
		return prComment{}, err
	}
	g = g2
	body, err = g.prepareBody(ctx, body)
	if err != nil {
		return prComment{}, err
	}
	if err := checkSummaryBody(body); err != nil {
		return prComment{}, err
	}
	return g.one(ctx, body, "--method", "POST", fmt.Sprintf("repos/{owner}/{repo}/issues/%d/comments", pr))
}

func (g ghForge) Update(ctx context.Context, id int64, body string) (prComment, error) {
	g2, err := g.resolve(ctx)
	if err != nil {
		return prComment{}, err
	}
	g = g2
	body, err = g.prepareBody(ctx, body)
	if err != nil {
		return prComment{}, err
	}
	if err := checkSummaryBody(body); err != nil {
		return prComment{}, err
	}
	return g.one(ctx, body, "--method", "PATCH", fmt.Sprintf("repos/{owner}/{repo}/issues/comments/%d", id))
}

// The exact rendered body is data; only a secret shape stops it now (privacy kinds were already
// scrubbed upstream). A refusal never includes its matching value.
func checkSummaryBody(body string) error {
	if r := publishpolicy.Scrub(body, publishpolicy.Context{}, publishpolicy.Policy{Disabled: true}); r.Blocked {
		return fmt.Errorf("GitHub summary blocked (secret-in-github-text)")
	}
	return nil
}

// assignedTo reports whether the ledger assigned the result with this sha256 to path: a file that has these bytes is the result of some patch, only not the one that is posted.
func assignedTo(recs []record, path, sha string) bool {
	for _, r := range recs {
		if samePath(r.Artifact, path) && r.SHA256 == sha && (r.Event == "finished" || r.Event == "unavailable") {
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
	// the posted copy is the sanitized one (CRW-1549): it is scrubbed before the comparison, so the
	// stored comment and what it is compared to are the same text; the artifact itself is not
	// touched. When the forge is gh the repository it resolves is the policy's repository — the
	// target gh actually posts to, which may differ from the checkout's origin.
	var repo string
	var pol publishpolicy.Policy
	var perr error
	if gh, ok := f.(ghForge); ok {
		g2, gerr := gh.resolve(ctx)
		if gerr != nil {
			return gerr
		}
		f = g2
		repo = g2.repo
		pol, perr = publication.Policy(g2.repo, os.Getenv)
	} else {
		repo, pol, perr = publication.ForCheckout(ctx, cfg.Repo)
	}
	err = perr
	if err != nil {
		return err
	}
	pubctx := publishpolicy.HostContext()
	if cfg.Repo != "" {
		pubctx.Paths = append(pubctx.Paths, cfg.Repo)
	}
	raw := body
	body, err = publication.Prepare(repo, "pr_comment", "body", body, pubctx, pol, stderr)
	if err != nil {
		return secretRefusal(raw, err)
	}
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
