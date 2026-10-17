//go:build dev

package ci

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pluginversion"
)

// stageRevision writes the local marketplace an installation is made from (CRW-1582): the
// catalog the revision ships and a plugins/crw payload whose manifest names the derived
// installation version. The source is never touched: the payload is materialized from the
// revision's tree, and the destination must be a new directory outside the checkout. The staged
// artifact is itself checked as an installed payload before it is reported.
func (c *pluginChecker) stageRevision(revision, destination string) ([]string, report, error) {
	errs, sourceReport, err := c.checkRevision(revision)
	if err != nil {
		return nil, nil, err
	}
	if len(errs) > 0 {
		return errs, nil, nil
	}
	resolved, _ := sourceReport["resolved"].(string)
	release, payloadErrs, err := c.revisionPayload(resolved)
	if err != nil {
		return nil, nil, err
	}
	if len(payloadErrs) > 0 {
		return payloadErrs, nil, nil
	}
	stamped, derived, err := pluginversion.StampedPayload(sharedPayload(release))
	if err != nil {
		return nil, nil, fmt.Errorf("%s manifest: %w", manifestPath, err)
	}
	if derived != sourceReport["version"] {
		return nil, nil, fmt.Errorf("the staged version %s is not the checked report's %s", derived, sourceReport["version"])
	}
	catalog, err := c.gitText("show", resolved+":"+marketplacePath)
	if err != nil {
		return nil, nil, err
	}

	abs, err := filepath.Abs(destination)
	if err != nil {
		return nil, nil, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return nil, nil, err
	}
	dest := filepath.Join(parent, filepath.Base(abs))
	root, err := filepath.EvalSymlinks(c.root)
	if err != nil {
		return nil, nil, err
	}
	rel, err := filepath.Rel(root, dest)
	if err != nil {
		return nil, nil, err
	}
	if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return nil, nil, fmt.Errorf("--stage destination must be outside the source checkout")
	}
	if err := os.Mkdir(dest, 0o755); err != nil {
		return nil, nil, err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(dest)
		}
	}()

	pluginRoot := filepath.Join(dest, pluginRelative)
	for _, name := range stamped.Names() {
		if !filepath.IsLocal(name) {
			return nil, nil, fmt.Errorf("the payload names %q, not a local path", name)
		}
		file := stamped[name]
		path := filepath.Join(pluginRoot, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, nil, err
		}
		switch file.Mode {
		case "100644":
		case "100755":
		default:
			return nil, nil, fmt.Errorf("the payload ships %s with mode %s, not a regular file", name, file.Mode)
		}
		if err := os.WriteFile(path, file.Data, 0o644); err != nil {
			return nil, nil, err
		}
		if file.Mode == "100755" {
			if err := os.Chmod(path, 0o755); err != nil {
				return nil, nil, err
			}
		}
	}
	catalogPath := filepath.Join(dest, marketplacePath)
	if err := os.MkdirAll(filepath.Dir(catalogPath), 0o755); err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(catalogPath, []byte(catalog), 0o644); err != nil {
		return nil, nil, err
	}
	artifactErrs, result, err := checkInstalled(pluginRoot)
	if err != nil {
		return nil, nil, err
	}
	if len(artifactErrs) > 0 {
		return artifactErrs, nil, nil
	}
	result["source"] = "staged"
	result["path"] = dest
	result["pluginPath"] = pluginRoot
	result["resolved"] = resolved
	result["sourceVersion"] = sourceReport["sourceVersion"]
	complete = true
	return nil, result, nil
}
