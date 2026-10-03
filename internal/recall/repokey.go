package recall

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// ReadOriginUrl is the origin-reader injection point of recall/src/repo-key.ts:19.
// Empty strings represent null; pack never produces an empty non-null key.
type ReadOriginUrl func(string) string

const GIT_TIMEOUT_MS = 1500
const originOutputLimit = 1 << 20 // spawnSync's default maxBuffer; stderr is ignored.

func normalizeRepoKey(raw string) string {
	raw = text.Trim(raw)
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, "://") {
		host, path, ok := parseRemoteURL(raw)
		if !ok {
			return ""
		}
		return pack(host, path)
	}
	// The optional userinfo can backtrack away: @ is legal in the host alternative.
	if at := strings.IndexByte(raw, '@'); at > 0 && !strings.ContainsRune(raw[:at], '/') {
		valid := true
		for _, r := range raw[:at] {
			valid = valid && !isJSSpace(r)
		}
		if valid {
			if host, path, ok := scpRemote(raw[at+1:]); ok {
				return pack(host, path)
			}
		}
	}
	if host, path, ok := scpRemote(raw); ok {
		return pack(host, path)
	}
	return ""
}

func scpRemote(raw string) (host, path string, ok bool) {
	colon := strings.IndexByte(raw, ':')
	if colon <= 0 || colon == len(raw)-1 {
		return "", "", false
	}
	host, path = raw[:colon], raw[colon+1:]
	for _, r := range host {
		if r == '/' || isJSSpace(r) {
			return "", "", false
		}
	}
	if strings.ContainsAny(path, "\n\r\u2028\u2029") {
		return "", "", false
	}
	return host, path, true
}

func pack(host, path string) string {
	host = Lower(text.Trim(host))
	path = strings.Trim(strings.ReplaceAll(text.Trim(path), "\\", "/"), "/")
	if len(path) >= 4 && strings.ToLower(path[len(path)-4:]) == ".git" {
		path = path[:len(path)-4]
	}
	path = strings.TrimRight(path, "/")
	if host == "" || path == "" {
		return ""
	}
	return host + "/" + path
}

// originOutput bounds memory and cancels only the command this reader started.
type originOutput struct {
	buffer bytes.Buffer
	cancel context.CancelFunc
}

func (b *originOutput) Write(p []byte) (int, error) {
	if len(p) > originOutputLimit-b.buffer.Len() {
		b.cancel()
		return 0, io.ErrShortBuffer
	}
	return b.buffer.Write(p)
}

// All subprocess failures are one empty result, preserving cwd-prefix fallback.
func readOriginUrl(cwd string) string {
	if text.Trim(cwd) == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), GIT_TIMEOUT_MS*time.Millisecond)
	defer cancel()
	out := originOutput{cancel: cancel}
	cmd := exec.CommandContext(ctx, "git", "-C", cwd, "config", "--get", "remote.origin.url")
	cmd.Stdout = &out
	cmd.WaitDelay = 50 * time.Millisecond // bound a pipe retained after the owned process exits.
	if cmd.Run() != nil || ctx.Err() != nil {
		return ""
	}
	return text.Trim(source.DecodeUTF8(out.buffer.Bytes()))
}

func repoKeyForCwd(cwd string, readers ...ReadOriginUrl) string {
	read := ReadOriginUrl(readOriginUrl)
	if len(readers) != 0 && readers[0] != nil {
		read = readers[0]
	}
	return normalizeRepoKey(read(cwd))
}

func repoKeysEqual(a, b string) bool { return a != "" && a == b }
