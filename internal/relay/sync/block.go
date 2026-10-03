// Package sync ports sync.py's durable coordination-document outbox.
package sync

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

type Obj = contract.OrderedObject

func obj(pairs ...any) Obj {
	out := make(Obj, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, contract.Field{Key: pairs[i].(string), Value: pairs[i+1]})
	}
	return out
}
func text(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
func hash(s string) string            { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) }
func StartMarker(id string) string    { return "<!-- relay-sync:" + id + " -->" }
func EndMarker(id string) string      { return "<!-- /relay-sync:" + id + " -->" }
func ContainerStart(id string) string { return "<!-- relay-sync-container:" + id + " -->" }
func ContainerEnd(id string) string   { return "<!-- /relay-sync-container:" + id + " -->" }
func CanonicalSummary(s string) string {
	return strings.TrimRight(strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n"), "\n")
}
func SummaryFence(summary string) string {
	longest, current := 0, 0
	for _, c := range summary {
		if c == '`' {
			current++
		} else {
			current = 0
		}
		longest = max(longest, current)
	}
	return strings.Repeat("`", max(3, longest+1))
}

var identityFields = []string{"syncId", "subjectKind", "issueKey", "relationshipId", "eventId", "executionGeneration", "revisionHash", "disposition", "identityDigest"}
var renderedFields = []string{"blockFormat", "syncId", "subjectKind", "issueKey", "relationshipId", "eventId", "executionGeneration", "revisionHash", "disposition", "identityDigest", "summarySha256"}

func headers(row store.Row) Obj {
	return obj("blockFormat", "v2", "syncId", row.Get("sync_id"), "subjectKind", row.Get("subject_kind"), "issueKey", row.Get("issue_key"), "relationshipId", row.Get("relationship_id"), "eventId", row.Get("event_id"), "executionGeneration", row.Get("execution_generation"), "revisionHash", row.Get("revision_hash"), "disposition", row.Get("verdict"), "identityDigest", row.Get("identity_digest"), "summarySha256", hash(CanonicalSummary(text(row.Get("summary")))))
}
func RenderBlock(row store.Row) string {
	id := text(row.Get("sync_id"))
	summary := CanonicalSummary(text(row.Get("summary")))
	fence := SummaryFence(summary)
	lines := []string{StartMarker(id)}
	for _, f := range headers(row) {
		lines = append(lines, f.Key+": "+text(f.Value))
	}
	lines = append(lines, "", fence+"text")
	lines = append(lines, strings.Split(summary, "\n")...)
	lines = append(lines, fence, EndMarker(id))
	return strings.Join(lines, "\n")
}
func fenceLength(line string) (int, string) {
	s := strings.TrimSpace(line)
	n := len(s) - len(strings.TrimLeft(s, "`"))
	if n < 3 || strings.Contains(s[n:], "`") {
		return 0, ""
	}
	return n, s[n:]
}

type Block struct {
	Fields                Obj
	Text, Summary, Format string
	// Problems (and PayloadMismatch's) are stored as sync_outbox.last_error, so they keep the
	// repr() quoting they were always written with.
	Problems []string
}

type Document struct {
	Blocks                       map[string]Block
	Order, Duplicates, Malformed []string
}

func ParseDocument(source string) Document {
	d := Document{Blocks: map[string]Block{}, Order: []string{}, Duplicates: []string{}, Malformed: []string{}}
	lines := strings.Split(source, "\n")
	for i := 0; i < len(lines); {
		stripped := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(stripped, "<!-- relay-sync:") || !strings.HasSuffix(stripped, "-->") {
			i++
			continue
		}
		id := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(stripped, "<!-- relay-sync:"), "-->"))
		closing := EndMarker(id)
		start := i
		i++
		b := Block{Fields: Obj{}, Format: "legacy", Problems: []string{}}
		seen := map[string]bool{}
		for i < len(lines) && strings.TrimSpace(lines[i]) != "" && strings.TrimSpace(lines[i]) != closing {
			key, value, ok := strings.Cut(lines[i], ":")
			key = strings.TrimSpace(key)
			if ok && key != "" && !strings.Contains(key, " ") {
				if seen[key] {
					b.Problems = append(b.Problems, "duplicate header "+pyvalue.StrRepr(key))
				}
				seen[key] = true
				found := false
				for j := range b.Fields {
					if b.Fields[j].Key == key {
						b.Fields[j].Value = strings.TrimSpace(value)
						found = true
					}
				}
				if !found {
					b.Fields = append(b.Fields, contract.Field{Key: key, Value: strings.TrimSpace(value)})
				}
			} else {
				b.Problems = append(b.Problems, "the header region contains a line that is not a header")
			}
			i++
		}
		for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
			i++
		}
		hasSummary := false
		if i < len(lines) {
			if n, _ := fenceLength(lines[i]); n > 0 {
				i++
				collected := []string{}
				closed := false
				for i < len(lines) {
					cn, info := fenceLength(lines[i])
					if cn >= n && info == "" {
						closed = true
						i++
						break
					}
					collected = append(collected, lines[i])
					i++
				}
				if closed {
					b.Summary = strings.Join(collected, "\n")
					hasSummary = true
				} else {
					b.Problems = append(b.Problems, "the fenced summary is not closed")
				}
				b.Format = "fenced-legacy"
			}
		}
		if declared := b.Fields.Get("blockFormat"); declared != nil {
			if declared != "v2" {
				b.Format = "unsupported"
				b.Problems = append(b.Problems, "unsupported block format "+pyvalue.StrRepr(text(declared)))
			} else {
				b.Format = "v2"
				if !hasSummary {
					b.Problems = append(b.Problems, "a v2 block must carry a closed fenced summary")
				}
			}
		}
		for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
			i++
		}
		if i >= len(lines) || strings.TrimSpace(lines[i]) != closing {
			tail := strings.Join(lines[start:], "\n")
			offset := strings.Index(tail, closing)
			if offset < 0 {
				if !slices.Contains(d.Malformed, id) {
					d.Malformed = append(d.Malformed, id)
				}
				i = start + 1
				continue
			}
			consumed := tail[:offset+len(closing)]
			body := consumed[strings.Index(consumed, "-->")+3 : len(consumed)-len(closing)]
			if hasSummary {
				b.Problems = append(b.Problems, "the block carries content between its fenced summary and its end marker")
			} else {
				b.Summary = strings.Trim(body, "\n")
			}
			b.Text = consumed
			i = start + strings.Count(consumed, "\n") + 1
		} else {
			if !hasSummary {
				b.Summary = strings.Trim(strings.Join(lines[start+1:i], "\n"), "\n")
			}
			b.Text = strings.Join(lines[start:i+1], "\n")
			i++
		}
		if _, ok := d.Blocks[id]; ok {
			if !slices.Contains(d.Duplicates, id) {
				d.Duplicates = append(d.Duplicates, id)
			}
		} else {
			d.Order = append(d.Order, id)
		}
		d.Blocks[id] = b
	}
	return d
}
func PayloadMismatch(row store.Row, b Block) []string {
	problems := append([]string{}, b.Problems...)
	if b.Format == "unsupported" {
		return problems
	}
	fields := identityFields
	if b.Format == "v2" {
		fields = renderedFields
	}
	expected := headers(row)
	for _, k := range fields {
		actual := b.Fields.Get(k)
		want := text(expected.Get(k))
		if actual == nil {
			problems = append(problems, k+" is missing")
		} else if actual != want {
			problems = append(problems, fmt.Sprintf("%s is %s, expected %s", k, pyvalue.StrRepr(text(actual)), pyvalue.StrRepr(want)))
		}
	}
	if b.Format == "v2" {
		unexpected := []string{}
		for _, f := range b.Fields {
			if !slices.Contains(renderedFields, f.Key) {
				unexpected = append(unexpected, f.Key)
			}
		}
		slices.Sort(unexpected)
		if len(unexpected) > 0 {
			items := make([]any, len(unexpected))
			for i, v := range unexpected {
				items[i] = v
			}
			problems = append(problems, "the header region carries unexpected headers "+pyvalue.Repr(items))
		}
		if b.Summary != CanonicalSummary(text(row.Get("summary"))) {
			problems = append(problems, "the fenced summary is not this job's summary text")
		}
	} else if summary := strings.TrimSpace(text(row.Get("summary"))); summary != "" && !strings.Contains(b.Summary, summary) {
		problems = append(problems, "the block does not carry this job's summary text")
	}
	return problems
}
