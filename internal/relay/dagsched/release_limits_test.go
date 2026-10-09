package dagsched

import (
	"fmt"
	"strings"
	"testing"
)

// releaseLimitRequest builds a dag-release request whose lists hold the given numbers of entries.
func releaseLimitRequest(criteria, roots, recipients, volatile int) []byte {
	list := func(n int, entry func(i int) string) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = entry(i)
		}
		return "[" + strings.Join(parts, ",") + "]"
	}
	return []byte(fmt.Sprintf(`{
	  "schema": "dag-release-request/1",
	  "base": {"repository": "owner/repo", "ref": "dev"},
	  "rule_version": {"skills_digest": "d1", "model": "gpt-5", "effort": "high", "prompt_template": "t1", "relay_build": "b1"},
	  "volatile": %s,
	  "instructions": "Do the work.",
	  "criteria": %s,
	  "criteria_source": "issue:CRW-1", "scope_ref": "issue:CRW-1",
	  "artifact_roots": %s, "allowed_recipients": %s,
	  "parent": {"host_id": "host", "settings": {"model": "gpt-5"}},
	  "child": {"host_id": "host", "title": "Child", "settings": {"model": "gpt-5"}}
	}`,
		list(volatile, func(i int) string {
			return fmt.Sprintf(`{"source": "file:%d", "snapshot_uri": "/tmp/x/%d.md", "sha256": "abc", "captured_at": "2026-10-02T00:00:00Z"}`, i, i)
		}),
		list(criteria, func(i int) string { return fmt.Sprintf(`{"id": "c%d", "title": "works", "required": true}`, i) }),
		list(roots, func(i int) string { return fmt.Sprintf(`"/tmp/x/%d"`, i) }),
		list(recipients, func(i int) string { return fmt.Sprintf(`"r%d"`, i) })))
}

// CRW-1046: a list over its bound is refused with the list's name, the number of entries it holds and the bound, not with one sentence about all four lists.
func TestDecodeReleaseRequestNamesTheListOverItsBound(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name                                  string
		criteria, roots, recipients, volatile int
		want                                  string
	}{
		{"criteria", 257, 1, 0, 0, "criteria has 257 entries; the limit is 256"},
		{"criteria empty", 0, 1, 0, 0, "criteria has no entry; at least one is required"},
		{"artifact_roots", 1, 65, 0, 0, "artifact_roots has 65 entries; the limit is 64"},
		{"artifact_roots empty", 1, 0, 0, 0, "artifact_roots has no entry; at least one is required"},
		{"allowed_recipients", 1, 1, 65, 0, "allowed_recipients has 65 entries; the limit is 64"},
		{"volatile", 1, 1, 0, releaseVolatileLimit + 1, fmt.Sprintf("volatile has %d entries; the limit is %d", releaseVolatileLimit+1, releaseVolatileLimit)},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := DecodeReleaseRequest(releaseLimitRequest(c.criteria, c.roots, c.recipients, c.volatile))
			if refusalReason(err) != "malformed_receipt" || err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want malformed_receipt saying %q", err, c.want)
			}
		})
	}
}

// CRW-1046: the 72 snapshot files of the report (every file its own sha256 in the manifest) are inside the bound, and so is the bound itself.
func TestDecodeReleaseRequestTakesSeventyTwoAndTheBoundOfVolatileEntries(t *testing.T) {
	t.Parallel()
	for _, n := range []int{72, releaseVolatileLimit} {
		req, err := DecodeReleaseRequest(releaseLimitRequest(1, 1, 0, n))
		if err != nil || len(req.Volatile) != n {
			t.Fatalf("%d volatile entries: %v, %d read", n, err, len(req.Volatile))
		}
	}
}
