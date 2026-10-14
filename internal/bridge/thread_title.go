package bridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/gitprobe"
)

// CRW-1313: the session that creates a thread names it right after thread/start with thread/name/set, by the thread
// title table of crw-canon/references/hierarchy.md ("Thread titles"), so the Codex app never shows the first prompt
// sentence in its place. A creation of the repository session (relay role supervisor) that names no title takes the
// table's title from its checkout's origin; a project session (relay role parent) and an issue child (relay role child)
// carry the title their creator wrote by the rule the table links (crw-plan's app presentation, the managed start's
// issue key prefix). A naming failure is warned and recorded in the receipt and never refuses the creation (Jun
// 10-14: a reversible step warns and records).

// titleWarningOutput is where a naming warning is written; a test swaps it.
var titleWarningOutput io.Writer = os.Stderr

// repositorySessionTitlePrefix is the table's title of the repository session before its owner/repo.
const repositorySessionTitlePrefix = "[CRW] 부모 · "

var slugPart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// repositorySessionTitle is the repository session's title for a checkout's origin URL: https://host/owner/repo[.git],
// ssh://[user@]host/owner/repo[.git] or the scp form [user@]host:owner/repo[.git]. A local path, a file URL, a path of
// more or fewer than two segments or anything else names no repository and gives "".
func repositorySessionTitle(origin string) string {
	origin = strings.TrimSpace(origin)
	var path string
	switch {
	case origin == "", strings.HasPrefix(origin, "/"), strings.HasPrefix(origin, "."):
		return ""
	case strings.Contains(origin, "://"):
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http" && u.Scheme != "ssh" && u.Scheme != "git") {
			return ""
		}
		path = u.Path
	default:
		host, rest, ok := strings.Cut(origin, ":")
		if !ok || host == "" {
			return ""
		}
		path = rest
	}
	owner, name, ok := strings.Cut(strings.TrimSuffix(strings.Trim(path, "/"), ".git"), "/")
	for _, part := range []string{owner, name} {
		if !ok || !slugPart.MatchString(part) || part == "." || part == ".." {
			return ""
		}
	}
	return repositorySessionTitlePrefix + owner + "/" + name
}

// checkoutOrigin is the origin URL of the checkout at cwd, or "" when it has none.
func checkoutOrigin(ctx context.Context, cwd string) string {
	bound, cancel := context.WithTimeout(ctx, gitprobe.Timeout)
	defer cancel()
	out, err := gitprobe.Command(bound, cwd, nil, "remote", "get-url", "origin").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// creationTitle is the name a creation gives its thread and, when it can give none by the table, the warning that
// says so. A creation with no role and no title keeps the behaviour it had: no name and no warning.
func creationTitle(ctx context.Context, role, title, cwd string) (string, map[string]any) {
	if title != "" {
		return title, nil
	}
	const after = ", so the thread keeps no name and the Codex app shows its first prompt sentence instead; name it by the thread title table of crw-canon/references/hierarchy.md"
	switch role {
	case "supervisor":
		if name := repositorySessionTitle(checkoutOrigin(ctx, cwd)); name != "" {
			return name, nil
		}
		return "", titleWarning("title_unset", "the repository session's checkout has no origin that names owner/repo"+after)
	case "parent":
		return "", titleWarning("title_unset", "a project session was created without a title"+after)
	case "child":
		return "", titleWarning("title_unset", "an issue child was created without a title"+after)
	}
	return "", nil
}

func titleWarning(code, message string) map[string]any {
	return map[string]any{"code": code, "message": message}
}

// nameThread gives the created thread its name. A host that refuses thread/name/set leaves the thread unnamed and the
// warning in the receipt, and the creation goes on to its first turn; a transport failure stays the creation's
// failure, because whether the host applied the name is then unknown and the replay of the request settles it.
func (b *Bridge) nameThread(ctx context.Context, threadID, name string, effects *[]string, receipt map[string]any) error {
	_, err := b.dispatch(ctx, "thread/name/set", map[string]any{"threadId": threadID, "name": name}, effects)
	var refused *appserver.RPCError
	switch {
	case errors.As(err, &refused):
		recordTitleWarning(receipt, titleWarning("title_not_set", "thread/name/set was refused ("+refused.Message+"); the thread keeps no name and the Codex app shows its first prompt sentence instead"))
		return nil
	case err != nil:
		return err
	}
	receipt["title"] = name
	return nil
}

// recordTitleWarning writes the warning into the receipt and to the warning output.
func recordTitleWarning(receipt map[string]any, warning map[string]any) {
	receipt["titleWarning"] = warning
	fmt.Fprintf(titleWarningOutput, "crw bridge: warning: %s: %s\n", warning["code"], warning["message"])
}
