package cli

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"go/version"
	"os"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
)

// FactVerifier is the preflight fact of the verifier's prerequisites (CRW-1319): whether the run's verifier can start crw-dev ci
// local with the tools the configuration names (or PATH, when it names none). It is never more than a warning: a verifier that
// does not start holds its node with the reason when it runs, and nothing is lost by starting.
const FactVerifier = "verifier_prerequisites"

// VerifierPrerequisites judges the verifier's prerequisites for a run started with env whose worktrees come from repo
// (crwconfig.ProbeVerifierTools, the probe crw doctor's verifierTools entry reads): crw-dev runs (in tree mode: its go runs and
// repo carries cmd/crw-dev), and the go is not older than the go version repo's go.mod asks. It passes when all hold and is a
// warning with the reasons otherwise; a configuration whose verifier section cannot be used is a warning too.
func VerifierPrerequisites(ctx context.Context, env []string, repo string) Fact {
	p := crwconfig.ProbeVerifierTools(ctx, env, "")
	problems := append([]string{}, p.Problems...)
	if p.Tools.VerifierKind() == crwconfig.VerifierGitHubActions {
		// the remote verifier needs no local Go or crw-dev: its prerequisites are the probe's own (CRW-1594)
		reading := fmt.Sprintf("github-actions %s workflow %q, runnable %t", p.Tools.Repository, p.Tools.GitHubWorkflow(), p.Runnable)
		if len(p.Notes) > 0 {
			reading += "; " + strings.Join(p.Notes, "; ")
		}
		if len(problems) == 0 {
			return Fact{Name: FactVerifier, Level: LevelPass, Pass: true, Reading: reading}
		}
		return Fact{Name: FactVerifier, Level: LevelWarn, Reading: reading, Reason: strings.Join(problems, "; ")}
	}
	if repo != "" && p.Mode == "tree" {
		if info, err := os.Stat(filepath.Join(repo, "cmd", "crw-dev")); err != nil || !info.IsDir() {
			problems = append(problems, fmt.Sprintf("the repository %s has no cmd/crw-dev for a tree verification to run: set verifier.crw_dev to an installed crw-dev (crw config verifier --crw-dev <path>)", repo))
		}
	}
	if want := goModVersion(repo); want != "" && version.IsValid(p.GoVersion) && version.Compare(p.GoVersion, "go"+want) < 0 {
		problems = append(problems, fmt.Sprintf("go %s is %s, older than the go %s the repository's go.mod asks, and the verifier runs it with GOTOOLCHAIN=local: set verifier.go_root to a newer Go toolchain root", p.Go, p.GoVersion, want))
	}
	goText := "no go"
	if p.Go != "" {
		goText = fmt.Sprintf("go %s %s from %s", p.Go, cell(p.GoVersion), p.GoSource)
	}
	reading := fmt.Sprintf("crw-dev %s (%s), runnable %t; %s", p.Mode, cell(strings.Join(p.Command, " ")), p.Runnable, goText)
	if len(p.Notes) > 0 {
		reading += "; " + strings.Join(p.Notes, "; ")
	}
	if len(problems) == 0 {
		return Fact{Name: FactVerifier, Level: LevelPass, Pass: true, Reading: reading}
	}
	return Fact{Name: FactVerifier, Level: LevelWarn, Reading: reading, Reason: strings.Join(problems, "; ")}
}

// goModVersion is the go version repo's go.mod names in its go directive, or "".
func goModVersion(repo string) string {
	if repo == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(repo, "go.mod"))
	if err != nil {
		return ""
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		if fields := strings.Fields(sc.Text()); len(fields) == 2 && fields[0] == "go" {
			return fields[1]
		}
	}
	return ""
}
