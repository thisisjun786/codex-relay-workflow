package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"
)

// scanBlobs checks every newly reachable blob, including files deleted before
// HEAD. It runs for attachments too: hosted jobs never substitute for range checks.
func (g *ghVerify) scanBlobs(ctx context.Context, wt, base, head string) error {
	const limit = 2 << 20
	var doc struct {
		Entries []struct {
			Path   string `json:"path"`
			Max    int64  `json:"max_bytes"`
			Reason string `json:"reason"`
		} `json:"entries"`
	}
	raw, found, err := g.commitFile(ctx, wt, head, ".large-blob-allowlist.json")
	if err != nil {
		return err
	}
	allow := map[string]int64{}
	if found {
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.DisallowUnknownFields()
		if err = dec.Decode(&doc); err != nil || doc.Entries == nil {
			return fmt.Errorf("invalid large-blob allowlist")
		}
		for _, e := range doc.Entries {
			if e.Path == "" || path.Clean(e.Path) != e.Path || path.IsAbs(e.Path) || strings.HasPrefix(e.Path, "../") || e.Max <= limit || strings.TrimSpace(e.Reason) == "" || allow[e.Path] != 0 {
				return fmt.Errorf("invalid large-blob allowance")
			}
			allow[e.Path] = e.Max
		}
	}
	objects, err := g.git(ctx, wt, "rev-list", "--objects", "--filter=blob:limit=2097153", "--filter-print-omitted", base+".."+head, "--")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(objects, "\n") {
		if !strings.HasPrefix(line, "~") {
			continue
		}
		oid := strings.TrimPrefix(line, "~")
		if !ghHex.MatchString(oid) {
			return fmt.Errorf("invalid omitted blob identity")
		}
		sizeText, err := g.git(ctx, wt, "cat-file", "-s", oid)
		if err != nil {
			return err
		}
		size, err := strconv.ParseInt(sizeText, 10, 64)
		if err != nil {
			return err
		}
		commits, err := g.git(ctx, wt, "log", "--format=%H", "--reverse", "--find-object="+oid, base+".."+head, "--")
		if err != nil {
			return err
		}
		ids := strings.Fields(commits)
		if len(ids) == 0 {
			return fmt.Errorf("large blob %s has no introducer", oid)
		}
		listing, err := g.git(ctx, wt, "ls-tree", "-r", "-z", ids[0])
		if err != nil {
			return err
		}
		seen := false
		for _, entry := range strings.Split(listing, "\x00") {
			fields, name, ok := strings.Cut(entry, "\t")
			if !ok {
				continue
			}
			parts := strings.Fields(fields)
			if len(parts) != 3 || parts[2] != oid {
				continue
			}
			seen = true
			if allow[name] < size {
				return fmt.Errorf("range %s..%s introduces blob %s over 2 MiB at %q", base, head, oid, name)
			}
		}
		if !seen {
			return fmt.Errorf("large blob %s has no allowed path", oid)
		}
	}
	return nil
}
