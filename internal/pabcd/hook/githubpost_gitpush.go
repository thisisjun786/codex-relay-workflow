package hook

import (
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/shellir"
)

func githubPostGitPush(args []shellir.Word) bool {
	for index := 0; index < len(args); index++ {
		if !args[index].Known {
			return true
		}
		value := args[index].Value
		if value == "-C" || value == "-c" || value == "--git-dir" || value == "--work-tree" || value == "--namespace" || value == "--super-prefix" {
			index++
			continue
		}
		if strings.HasPrefix(value, "-") {
			continue
		}
		return value == "push"
	}
	return false
}
