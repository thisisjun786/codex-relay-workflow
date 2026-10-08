package hook

import (
	"os"
	"path/filepath"
)

// shellirPayloadCwd is the directory the command readers judge a command in: the payload's cwd when it is an absolute path
// to an existing directory, and "" otherwise. An empty, relative or missing cwd makes the directory unknown, so a relative
// target is never resolved against the hook's own process directory or taken as the literal path (CRW-1028 item 7).
func shellirPayloadCwd(cwd string) string {
	if cwd == "" || !filepath.IsAbs(cwd) {
		return ""
	}
	st, err := os.Stat(cwd)
	if err != nil || !st.IsDir() {
		return ""
	}
	return cwd
}
