package dagsched

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// The node pin of a commit is the node-version its ci.yml gives each actions/setup-node step (CRW-1026, d1). The workflow is
// read as the block structure YAML gives it, so a step is exactly one item of a job's steps list: its keys are the item's
// own, a quoted value is the same value as a plain one, and nothing after the item (a later step, a key of the job, another
// job) belongs to it. The reader covers the block mappings, block sequences, plain and quoted scalars and block scalars a
// workflow is written in; it is not a general YAML parser, and a workflow that mentions actions/setup-node in a shape it
// does not read is an error, never "no Node declaration".

// workflowNodeVersions are the distinct node-version values of the jobs' setup-node steps, sorted. A setup-node step whose
// own with block names no node-version this reader can read, a setup-node use that is not a step it reads (a flow
// collection, a job-level uses, an anchor), and a workflow it cannot read at all while it mentions actions/setup-node are
// errors. A workflow that never mentions actions/setup-node declares no node, whatever its shape.
func workflowNodeVersions(text string) ([]string, error) {
	mentions := strings.Contains(strings.ToLower(text), pinSetupNodeAction)
	root, err := parseWorkflowBlocks(text)
	if err != nil {
		if !mentions {
			return nil, nil
		}
		return nil, refuse(contract.RefusalDispositionConflict, "ci.yml mentions actions/setup-node but this reader cannot read its structure (%v), so the node pin the commit declares is unknown", err)
	}
	var versions []string
	read := 0
	if jobs := root.get("jobs"); jobs != nil && jobs.kind == workflowMapping {
		for _, job := range jobs.values {
			if job.kind != workflowMapping {
				continue
			}
			steps := job.get("steps")
			if steps == nil || steps.kind != workflowSequence {
				continue
			}
			for _, step := range steps.items {
				if step.kind != workflowMapping || !isSetupNodeUse(step.get("uses")) {
					continue
				}
				read++
				var version *workflowNode
				if with := step.get("with"); with != nil && with.kind == workflowMapping {
					version = with.get("node-version")
				}
				if version == nil || version.kind != workflowScalar || strings.TrimSpace(version.value) == "" {
					return nil, refuse(contract.RefusalDispositionConflict, "ci.yml has an actions/setup-node step with no node-version this reader can read, so the node pin the commit declares is unknown")
				}
				versions = append(versions, strings.TrimSpace(version.value))
			}
		}
	}
	// every use of setup-node the file holds must be a step read above: a use elsewhere, or in a form the reader keeps only as
	// raw text, would otherwise drop a node declaration silently
	if uses := setupNodeUses(root); uses != read {
		return nil, refuse(contract.RefusalDispositionConflict, "ci.yml uses actions/setup-node %d times but only %d are steps this reader can read, so the node pin the commit declares is unknown", uses, read)
	}
	slices.Sort(versions)
	return slices.Compact(versions), nil
}

// pinSetupNodeAction is the action whose steps declare the node pin, lowercased (an action reference is case-insensitive).
const pinSetupNodeAction = "actions/setup-node"

// isSetupNodeUse is whether a uses value names actions/setup-node at some ref.
func isSetupNodeUse(uses *workflowNode) bool {
	return uses != nil && uses.kind == workflowScalar && strings.HasPrefix(strings.ToLower(strings.TrimSpace(uses.value)), pinSetupNodeAction+"@")
}

// setupNodeUses counts the uses entries of any mapping, at any depth, that name actions/setup-node, and every value the
// reader keeps only as raw text (a flow collection, an alias, a tag, a block scalar header) that mentions it.
func setupNodeUses(n *workflowNode) int {
	if n == nil {
		return 0
	}
	count := 0
	switch n.kind {
	case workflowMapping:
		for i, key := range n.keys {
			value := n.values[i]
			if key == "uses" && isSetupNodeUse(value) {
				count++
				continue
			}
			count += setupNodeUses(value)
		}
	case workflowSequence:
		for _, item := range n.items {
			count += setupNodeUses(item)
		}
	case workflowRaw:
		if strings.Contains(strings.ToLower(n.value), pinSetupNodeAction) {
			count++
		}
	}
	return count
}

// workflowNode is one node of the block structure: a mapping (keys and values in order), a sequence, a scalar (value is
// the scalar with its quotes and trailing comment removed), or raw text the reader does not interpret (value as written).
type workflowNode struct {
	kind   workflowKind
	keys   []string
	values []*workflowNode
	items  []*workflowNode
	value  string
}

type workflowKind int

const (
	workflowScalar workflowKind = iota
	workflowMapping
	workflowSequence
	workflowRaw
)

// get is the value of a mapping's key, nil when the node is not a mapping or has no such key.
func (n *workflowNode) get(key string) *workflowNode {
	if n == nil || n.kind != workflowMapping {
		return nil
	}
	for i, k := range n.keys {
		if k == key {
			return n.values[i]
		}
	}
	return nil
}

// workflowLine is one line that carries content: its indentation in spaces and the text after it.
type workflowLine struct {
	indent int
	text   string
	number int
}

type workflowParser struct {
	lines []workflowLine
	at    int
}

// parseWorkflowBlocks reads the block structure of a workflow. Blank lines and comment lines carry nothing; a line deeper or
// shallower than the structure allows is an error.
func parseWorkflowBlocks(text string) (*workflowNode, error) {
	p := &workflowParser{}
	for n, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimLeft(line, " ")
		content := strings.TrimSpace(trimmed)
		if content == "" || strings.HasPrefix(content, "#") {
			continue
		}
		if len(p.lines) == 0 && content == "---" {
			continue
		}
		p.lines = append(p.lines, workflowLine{indent: len(line) - len(trimmed), text: strings.TrimRight(trimmed, " \t"), number: n + 1})
	}
	if len(p.lines) == 0 {
		return &workflowNode{kind: workflowMapping}, nil
	}
	if p.lines[0].indent != 0 {
		return nil, fmt.Errorf("line %d: the document does not start at the left margin", p.lines[0].number)
	}
	root, err := p.block(0)
	if err != nil {
		return nil, err
	}
	if p.at < len(p.lines) {
		return nil, fmt.Errorf("line %d: unexpected content after the document's structure", p.lines[p.at].number)
	}
	return root, nil
}

// block reads the node whose first line is the current one, at that line's indentation.
func (p *workflowParser) block(indent int) (*workflowNode, error) {
	line := p.lines[p.at]
	if strings.HasPrefix(line.text, "\t") {
		return nil, fmt.Errorf("line %d: a tab indents the line", line.number)
	}
	if isWorkflowItem(line.text) {
		return p.sequence(indent)
	}
	if _, _, ok := splitWorkflowKey(line.text); ok {
		return p.mapping(indent)
	}
	// a scalar on its own line: it must end there
	p.at++
	if p.at < len(p.lines) && p.lines[p.at].indent > indent {
		return nil, fmt.Errorf("line %d: a scalar continues on a deeper line", p.lines[p.at].number)
	}
	return workflowScalarOf(line.text), nil
}

// sequence reads the items of a block sequence at indent. An item's content starts after its dash, on the same line or on
// the deeper lines that follow; a mapping started after the dash continues at the column of its first key.
func (p *workflowParser) sequence(indent int) (*workflowNode, error) {
	n := &workflowNode{kind: workflowSequence}
	for p.at < len(p.lines) {
		line := p.lines[p.at]
		if line.indent != indent || !isWorkflowItem(line.text) {
			break
		}
		rest := strings.TrimLeft(line.text[1:], " ")
		if rest == "" || strings.HasPrefix(rest, "#") {
			p.at++
			item, err := p.child(indent)
			if err != nil {
				return nil, err
			}
			n.items = append(n.items, item)
			continue
		}
		column := indent + len(line.text) - len(rest)
		p.lines[p.at] = workflowLine{indent: column, text: rest, number: line.number}
		item, err := p.block(column)
		if err != nil {
			return nil, err
		}
		n.items = append(n.items, item)
	}
	return n, nil
}

// mapping reads the entries of a block mapping at indent. A key with no value on its line takes the deeper block that
// follows, or a sequence at the key's own indentation (the compact form steps: / - uses: ...).
func (p *workflowParser) mapping(indent int) (*workflowNode, error) {
	n := &workflowNode{kind: workflowMapping}
	for p.at < len(p.lines) {
		line := p.lines[p.at]
		if line.indent < indent || (line.indent == indent && isWorkflowItem(line.text)) {
			break
		}
		if line.indent > indent {
			return nil, fmt.Errorf("line %d: the line is deeper than the mapping it is in", line.number)
		}
		if strings.HasPrefix(line.text, "\t") {
			return nil, fmt.Errorf("line %d: a tab indents the line", line.number)
		}
		key, value, ok := splitWorkflowKey(line.text)
		if !ok {
			return nil, fmt.Errorf("line %d: a line of a mapping that is not a key", line.number)
		}
		p.at++
		var node *workflowNode
		switch {
		case value == "":
			if p.at < len(p.lines) && p.lines[p.at].indent == indent && isWorkflowItem(p.lines[p.at].text) {
				var err error
				if node, err = p.sequence(indent); err != nil {
					return nil, err
				}
				break
			}
			var err error
			if node, err = p.child(indent); err != nil {
				return nil, err
			}
		case isWorkflowBlockScalar(value):
			// a literal or folded block: its body is every deeper line, and none of it is structure
			for p.at < len(p.lines) && p.lines[p.at].indent > indent {
				p.at++
			}
			node = &workflowNode{kind: workflowRaw, value: value}
		default:
			if p.at < len(p.lines) && p.lines[p.at].indent > indent {
				return nil, fmt.Errorf("line %d: the value of %s continues on a deeper line", p.lines[p.at].number, key)
			}
			node = workflowScalarOf(value)
		}
		n.keys = append(n.keys, key)
		n.values = append(n.values, node)
	}
	return n, nil
}

// child reads the deeper block that follows a line, or an empty scalar when none does.
func (p *workflowParser) child(indent int) (*workflowNode, error) {
	if p.at < len(p.lines) && p.lines[p.at].indent > indent {
		return p.block(p.lines[p.at].indent)
	}
	return &workflowNode{kind: workflowScalar}, nil
}

// isWorkflowItem is whether a line starts a sequence item.
func isWorkflowItem(text string) bool {
	return text == "-" || strings.HasPrefix(text, "- ")
}

// isWorkflowBlockScalar is whether a value opens a literal or folded block scalar (|, >, with chomping or indentation
// indicators and an optional comment).
func isWorkflowBlockScalar(value string) bool {
	value = strings.TrimSpace(stripWorkflowComment(value))
	if value == "" || (value[0] != '|' && value[0] != '>') {
		return false
	}
	return strings.Trim(value[1:], "+-0123456789") == ""
}

// splitWorkflowKey splits a mapping entry into its key and the value text after it. A key is plain or quoted and ends at
// the first colon that a space or the end of the line follows; a flow collection or a scalar with no such colon is no key.
func splitWorkflowKey(text string) (string, string, bool) {
	if text == "" || text[0] == '{' || text[0] == '[' {
		return "", "", false
	}
	if text[0] == '"' || text[0] == '\'' {
		end := workflowQuoteEnd(text)
		if end < 0 {
			return "", "", false
		}
		key := workflowScalarOf(text[:end+1]).value
		rest := text[end+1:]
		if rest == ":" {
			return key, "", true
		}
		if strings.HasPrefix(rest, ": ") {
			return key, strings.TrimSpace(rest[2:]), true
		}
		return "", "", false
	}
	body := stripWorkflowComment(text)
	if i := strings.Index(body, ": "); i > 0 {
		return strings.TrimSpace(body[:i]), strings.TrimSpace(text[i+2:]), true
	}
	if trimmed := strings.TrimSpace(body); strings.HasSuffix(trimmed, ":") && len(trimmed) > 1 {
		return strings.TrimSpace(trimmed[:len(trimmed)-1]), "", true
	}
	return "", "", false
}

// workflowScalarOf reads a value written on one line. A quoted value loses its quotes (and its escapes), a plain value its
// trailing comment; a flow collection, an alias, an anchor or a tag is kept as raw text.
func workflowScalarOf(value string) *workflowNode {
	value = strings.TrimSpace(value)
	if value == "" {
		return &workflowNode{kind: workflowScalar}
	}
	switch value[0] {
	case '{', '[', '&', '*', '!', '|', '>':
		return &workflowNode{kind: workflowRaw, value: value}
	case '"', '\'':
		end := workflowQuoteEnd(value)
		if end < 0 {
			return &workflowNode{kind: workflowRaw, value: value}
		}
		if rest := strings.TrimSpace(value[end+1:]); rest != "" && !strings.HasPrefix(rest, "#") {
			return &workflowNode{kind: workflowRaw, value: value}
		}
		quoted := value[:end+1]
		if quoted[0] == '\'' {
			return &workflowNode{kind: workflowScalar, value: strings.ReplaceAll(quoted[1:len(quoted)-1], "''", "'")}
		}
		unquoted, err := strconv.Unquote(quoted)
		if err != nil {
			return &workflowNode{kind: workflowRaw, value: value}
		}
		return &workflowNode{kind: workflowScalar, value: unquoted}
	}
	return &workflowNode{kind: workflowScalar, value: strings.TrimSpace(stripWorkflowComment(value))}
}

// workflowQuoteEnd is the index of the quote that closes the quoted scalar text starts with, -1 when it is not closed on the
// line. A single-quoted scalar escapes a quote by doubling it, a double-quoted one with a backslash.
func workflowQuoteEnd(text string) int {
	quote := text[0]
	for i := 1; i < len(text); i++ {
		switch {
		case quote == '"' && text[i] == '\\':
			i++
		case text[i] == quote && quote == '\'' && i+1 < len(text) && text[i+1] == '\'':
			i++
		case text[i] == quote:
			return i
		}
	}
	return -1
}

// stripWorkflowComment drops a trailing comment from plain text: a # that starts the text or follows a space.
func stripWorkflowComment(text string) string {
	if strings.HasPrefix(text, "#") {
		return ""
	}
	if i := strings.Index(text, " #"); i >= 0 {
		return text[:i]
	}
	return text
}
