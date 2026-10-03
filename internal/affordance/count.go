// Package affordance ports CXC v0.2.40 cxc-ops/src/map-affordance.ts. These hooks
// advertise capabilities; they do not start a workflow or activate a hook.
package affordance

import (
	"os"
	"path/filepath"
	"strings"
)

const (
	MapAffordanceMinFiles = 40
	CountCap              = 60
	MaxDirsVisited        = 4000
)

func sourceExt(s string) bool {
	switch s {
	case ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".py", ".rs", ".go",
		".java", ".rb", ".c", ".h", ".cpp", ".hpp", ".cs", ".swift", ".kt",
		".scala", ".lua", ".ex", ".exs", ".php":
		return true
	}
	return false
}

func skipDir(s string) bool {
	if strings.HasPrefix(s, ".") {
		return true
	}
	switch s {
	case "node_modules", "dist", "build", "target", "out", "coverage",
		"__pycache__", "venv", "env", "vendor":
		return true
	}
	return false
}

// CountSourceFiles is the bounded breadth-first walk, including the root in the
// directory cap. ReadDir sorts names as Node's readdirSync/scandir does.
func CountSourceFiles(root string) int {
	count, queue := 0, []string{root}
	for visited := 0; visited < len(queue) && visited < MaxDirsVisited && count < CountCap; visited++ {
		entries, err := os.ReadDir(queue[visited])
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() && !skipDir(entry.Name()) {
				queue = append(queue, filepath.Join(queue[visited], entry.Name()))
			} else if entry.Type().IsRegular() {
				if dot := strings.LastIndex(entry.Name(), "."); dot > 0 && sourceExt(entry.Name()[dot:]) {
					count++
					if count == CountCap {
						break
					}
				}
			}
		}
	}
	return count
}
