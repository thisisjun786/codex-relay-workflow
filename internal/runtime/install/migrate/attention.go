package migrate

// attention.go is the read-only attention report of docs/port-cxc/state-migration.md "Keep embedded paths and recorded
// hashes": the copied records that still point at a source root, the hashes that change when a path changes, the
// multi-file non-ASCII freeze whose recorded aggregate is stale for Go's ordering, and the B and C sessions that need
// fresh evidence. Only a record matching a row of the typed table is opened and parsed, into a decoded view that is never
// serialized back, so no copied payload byte can change here. It advances no phase and bypasses no gate.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"unicode"
)

// The kinds of attention this report raises.
const (
	AttentionOldRoot     = "old-root-ref"
	AttentionPathHash    = "path-hash"
	AttentionFreezeOrder = "freeze-order"
	AttentionFreshness   = "session-freshness"
)

// attentionValueCap bounds the retained value a report carries; the record itself is never changed.
const attentionValueCap = 256

// attentionReadCap bounds the bytes of one record this report reads, so a very large artifact cannot exhaust memory.
const attentionReadCap = 1 << 20

// Attention is one report entry: the copied record, the listed field, the retained value and why it needs attention.
type Attention struct {
	Scope  Scope
	Item   string // the copied record's root-relative source path
	Kind   string
	Field  string
	Value  string
	Detail string
}

// attentionField is one listed field of the design's path-field table: a JSON key path, where "*" steps into every element
// of an array, and the kind of entry the value raises ("" scans the value for a source root).
type attentionField struct {
	path []string
	kind string
}

// attentionFields is the design's path-field table, by record kind. An arbitrary string stays opaque and is never scanned.
var attentionFields = map[string][]attentionField{
	"session": {{path: []string{"planUnit"}}, {path: []string{"boundSourceRoot"}}, {path: []string{"phaseEntrySource", "sourceRoot"}},
		{path: []string{"phase"}, kind: AttentionFreshness}},
	"attest": {{path: []string{"planUnit"}}, {path: []string{"planPaths", "*"}}, {path: []string{"testReceiptPath"}}},
	"goalplan": {{path: []string{"finalGate", "testReceiptPath"}}, {path: []string{"finalGate", "qaReceiptPath"}},
		{path: []string{"finalGate", "sourceIdentity", "sourceRoot"}}, {path: []string{"planFiles", "*", "path"}},
		{path: []string{"reviewRounds", "*", "planPath"}}, {path: []string{"reviewRounds", "*", "planUnit"}},
		{path: []string{"reviewRounds", "*", "planFiles", "*", "path"}}, {path: []string{"reviewRounds", "*", "lane", "workspaceRoot"}},
		{path: []string{"reviewRounds", "*", "lane", "sourceIdentity", "sourceRoot"}},
		{path: []string{"reviewRounds", "*", "planSha256"}, kind: AttentionPathHash}},
	"source": {{path: []string{"nativeCwd"}}, {path: []string{"sourceRoot"}}, {path: []string{"commonDir"}}, {path: []string{"gitDir"}}},
	"freeze": {{path: []string{"planFiles", "*", "path"}}, {path: []string{"evidenceBundle", "researchReportRef"}}},
	"evidence": {{path: []string{"sourceIdentity", "sourceRoot"}}, {path: []string{"generatedPaths", "*"}},
		{path: []string{"artifactManifest", "*", "path"}}, {path: []string{"artifactRefs", "*"}}},
	"divergence": {{path: []string{"worktree"}}, {path: []string{"sourceUrls", "*"}}},
	"render":     {{path: []string{"screenshotPath"}}},
	"bg":         {{path: []string{"cwd"}}, {path: []string{"command", "*"}}, {path: []string{"note"}}},
	"manifest": {{path: []string{"configPath"}}, {path: []string{"backupPath"}},
		{path: []string{"tableKeys", "*", "priorValue"}}, {path: []string{"tableKeys", "*", "appliedValue"}}},
}

// attention reports the listed records of a plan. A record that matches a row but cannot be read or decoded yields an
// entry naming the error, so a record the copy preserves byte for byte is never silently passed over.
func attention(r *Roots, plan *Plan) []Attention {
	a := &attentionRun{run: &applyRun{roots: r, srcs: map[string]*Dir{}}}
	defer a.run.close()
	if plan == nil {
		return nil
	}
	for _, p := range []*Pair{r.Project, r.User} {
		if p != nil {
			a.srcRoots = append(a.srcRoots, p.SourcePath)
		}
	}
	for _, it := range plan.Items {
		if applyWrites(it) && !applyDir(it) {
			if kind := attentionKind(it.Source); kind != "" {
				a.record(it.Scope, it.Source, kind)
			}
		}
	}
	return a.out
}

// attentionRun reads the listed records through M1's pinned, no-follow opener, shared with the write step.
type attentionRun struct {
	run      *applyRun
	srcRoots []string
	out      []Attention
}

// attentionKind names the typed row a copied file matches, or "" for a file this report does not read.
func attentionKind(path string) string {
	switch {
	case inventoryMatch("sessions/*.json", path):
		return "session"
	case path == "attest.json":
		return "attest"
	case inventoryMatch("goalplans/*/goalplan.json", path):
		return "goalplan"
	case inventoryMatch("sources/*.json", path):
		return "source"
	case path == "interview/freeze.json":
		return "freeze"
	case path == installSource:
		return "manifest"
	case path == "divergence/candidates.jsonl":
		return "divergence"
	case path == "render-observations.jsonl":
		return "render"
	case strings.HasPrefix(path, "evidence/") && strings.HasSuffix(path, ".json"):
		return "evidence"
	case inventoryMatch("bg/*.json", path):
		return "bg"
	}
	return ""
}

func (a *attentionRun) add(scope Scope, item, kind, field, value, detail string) {
	if len(value) > attentionValueCap {
		value = value[:attentionValueCap]
	}
	a.out = append(a.out, Attention{Scope: scope, Item: item, Kind: kind, Field: field, Value: value, Detail: detail})
}

// record reads one copied record and walks its listed fields. A .jsonl ledger is read one object per line.
func (a *attentionRun) record(scope Scope, path, kind string) {
	data, err := a.read(scope, path)
	if err != nil {
		a.add(scope, path, "", "", "", "the record cannot be read: "+err.Error())
		return
	}
	if !strings.HasSuffix(path, ".jsonl") {
		a.object(scope, path, kind, data)
		return
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) > 0 {
			a.object(scope, path, kind, line)
		}
	}
}

func (a *attentionRun) object(scope Scope, path, kind string, data []byte) {
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		a.add(scope, path, "", "", "", "the record cannot be decoded: "+err.Error())
		return
	}
	root, ok := decoded.(map[string]any)
	if !ok {
		a.add(scope, path, "", "", "", "the record is not a JSON object")
		return
	}
	for _, f := range attentionFields[kind] {
		attentionWalk(root, f.path, "", func(field, value string) {
			switch {
			case f.kind == AttentionFreshness:
				if value == "B" || value == "C" {
					a.add(scope, path, AttentionFreshness, field, value, "the session is in "+value+"; its next capture needs fresh CRW evidence")
				}
			case f.kind == AttentionPathHash:
				a.add(scope, path, AttentionPathHash, field, value, "the hash covers the path as well as the bytes, and the copied record keeps it unchanged")
			case a.oldRoot(value):
				a.add(scope, path, AttentionOldRoot, field, value, "the value names a source root; the copied payload keeps it unchanged")
			}
		})
	}
	if kind == "freeze" {
		a.freeze(scope, path, root)
	}
}

// attentionWalk follows a key path into a decoded record, calling f with the field label and the value it finds.
func attentionWalk(v any, path []string, label string, f func(field, value string)) {
	if len(path) == 0 {
		if s, ok := v.(string); ok {
			f(label, s)
		}
		return
	}
	if path[0] == "*" {
		switch node := v.(type) {
		case []any:
			for _, e := range node {
				attentionWalk(e, path[1:], label+".*", f)
			}
		case map[string]any:
			keys := make([]string, 0, len(node))
			for k := range node {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			for _, k := range keys {
				attentionWalk(node[k], path[1:], label+".*", f)
			}
		}
		return
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return
	}
	child, ok := obj[path[0]]
	if !ok {
		return
	}
	if next := path[0]; label != "" {
		attentionWalk(child, path[1:], label+"."+next, f)
	} else {
		attentionWalk(child, path[1:], next, f)
	}
}

// freeze reports the aggregate hash of a freeze that holds more than one plan file: the hash covers the sorted plan file
// names, and Go's non-ASCII ordering differs from the oracle's, so the recorded hash is reported unchanged and the freeze
// needs an explicit revalidation instead of a rehash (state-migration.md, "Hash consequences").
func (a *attentionRun) freeze(scope Scope, path string, root map[string]any) {
	files, _ := root["planFiles"].([]any)
	var paths []string
	for _, f := range files {
		if obj, ok := f.(map[string]any); ok {
			if s, ok := obj["path"].(string); ok {
				paths = append(paths, s)
			}
		}
	}
	hash, _ := root["planHash"].(string)
	if len(paths) < 2 || hash == "" {
		return
	}
	a.add(scope, path, AttentionPathHash, "planHash", hash, "the aggregate hash covers the sorted plan file names, so the copied set is path-sensitive")
	for _, p := range paths {
		if strings.IndexFunc(p, func(r rune) bool { return r > unicode.MaxASCII }) >= 0 {
			a.add(scope, path, AttentionFreezeOrder, "planFiles.*.path", hash, "a multi-file freeze with a non-ASCII name: Go's ordering can differ, so the recorded hash is kept and the freeze needs explicit revalidation")
			return
		}
	}
}

// oldRoot reports whether a retained value names a source root, which the copy keeps unchanged.
func (a *attentionRun) oldRoot(v string) bool {
	if strings.Contains(v, ProjectSourceName) {
		return true
	}
	for _, root := range a.srcRoots {
		if root != "" && strings.Contains(v, root) {
			return true
		}
	}
	return false
}

// read returns the bytes of a source record, read through M1's pinned, no-follow opener.
func (a *attentionRun) read(scope Scope, path string) ([]byte, error) {
	dir, err := a.run.open(scope, classifyDirPart(path), false)
	if err != nil {
		return nil, err
	}
	_, base := applySplit(path)
	f, _, err := dir.OpenRegular(base)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, attentionReadCap+1))
	if err != nil {
		return nil, err
	}
	if len(data) > attentionReadCap {
		return nil, errors.New("the record is larger than the attention view reads")
	}
	return data, nil
}
