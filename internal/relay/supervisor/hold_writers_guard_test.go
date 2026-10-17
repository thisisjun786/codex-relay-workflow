package supervisor

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestEveryHoldWriteAsksTheRule (CRW-1500 U10) is the writers guard of the hold rule: every write
// that can change a message's hold_reason is a site of plan-r3 section 3, of the class that table
// gives it, and asks the rule (holdDecision) or is safe by its own WHERE.
//
// It parses the non-test Go files of internal/relay/supervisor and internal/relay/store and collects,
// keyed by file, enclosing function and ordinal within it: every string-built statement that starts
// UPDATE supervisor_messages (all of them, not only those whose SET names hold_reason), every
// supervisor_messages INSERT, every journal INSERT of kind supervisor_message_superseded and every
// SettleSupervisorMessage call. An UPDATE whose SET names hold_reason must be one of
// holdWriteSites, with what its class requires; the UPDATEs listed in holdQuietUpdates must still not
// name it; any other UPDATE, a listed site that is gone, an INSERT naming hold_reason, a superseded
// journal insert outside its three writers or an unlisted settle caller fails by name. No other
// package under internal/relay writes supervisor_messages.
func TestEveryHoldWriteAsksTheRule(t *testing.T) {
	scan := scanHoldWrites(t, filepath.Join("..", "..", ".."))
	if os.Getenv("CRW_HOLD_WRITERS_DUMP") != "" {
		for _, s := range scan.sites {
			t.Logf("%s %s %s#%d set=%v\n\t%s", s.kind, s.file, s.fn, s.ordinal, s.setNamesHold(), strings.Join(s.variants, "\n\t| "))
		}
	}
	for _, problem := range checkHoldWrites(scan) {
		t.Error(problem)
	}
}

// holdWriteClass is a class of the section 3 table.
type holdWriteClass string

const (
	// classRule: the site asks holdDecision, directly or through the functions in via.
	classRule holdWriteClass = "rule"
	// classCASFromNone: a deferral that writes only over the hold it read, and asks the rule as defer
	// so that a held row is never overwritten.
	classCASFromNone holdWriteClass = "cas-from-none"
	// classOwnWord: the WHERE names one word that is not superseded_by_report (or no hold), so the
	// site can never touch a derived, acknowledged or notice hold.
	classOwnWord holdWriteClass = "own-word"
	// classStoreHalf: the store side of a rule cell, whose SQL is the cell (the settle CASE, the
	// acknowledgement's set from none).
	classStoreHalf holdWriteClass = "store-half"
)

// holdWriteSite is one row of the section 3 table: an UPDATE supervisor_messages whose SET names
// hold_reason. set and where are substrings (of the normalized SQL) that some variant of the SET
// and the WHERE must hold; via are the functions the site's function must call, each reaching
// holdDecision; entries are the holdChange entries holdDecision is called with in the site's
// function or in its first via; refs are names the site's function must use.
type holdWriteSite struct {
	file, fn string
	ordinal  int
	class    holdWriteClass
	entry    string // the rule entry or the word of the site, as the plan's table names it
	set      []string
	where    []string
	via      []string
	entries  []string
	refs     []string
}

const (
	sqlUnsent      = "SupervisorUnsentSQL("
	sqlNeverSent   = "SupervisorNeverSentSQL("
	sqlCASETwo     = "hold_reason=CASE WHEN hold_reason IN (?,?) THEN NULL ELSE hold_reason END"
	refSuperseded  = "store.SupervisorHoldSuperseded"
	refUnaddressed = "store.SupervisorHoldUnaddressed"
)

// holdWriteSites is plan-r3 section 3, row for row: the UPDATEs whose SET names hold_reason.
var holdWriteSites = []holdWriteSite{
	{file: "supervisor/stage.go", fn: "Channel.StageWithReading", ordinal: 1, class: classOwnWord, entry: "hierarchy_unresolved (the :158-169 release)",
		set: []string{"hold_reason=NULL"}, where: []string{"hold_reason='hierarchy_unresolved'", sqlUnsent}},
	{file: "supervisor/stage.go", fn: "Channel.StageWithReading", ordinal: 2, class: classRule, entry: "readdress",
		set: []string{"hold_reason=?"}, where: []string{sqlNeverSent}, via: []string{"holdDecision", "reopenProposal"}, entries: []string{"entryReaddress"}},
	{file: "supervisor/proposal.go", fn: "Channel.reopenAddressed", ordinal: 1, class: classOwnWord, entry: "hierarchy_unresolved",
		set: []string{"hold_reason=NULL"}, where: []string{"hold_reason='hierarchy_unresolved'", sqlUnsent}},
	{file: "supervisor/proposal.go", fn: "Channel.reopenProposal", ordinal: 1, class: classRule, entry: "restage or send_by_id (its caller's)",
		set: []string{"hold_reason=NULL"}, where: []string{"hold_reason='superseded_by_report'", sqlUnsent}, via: []string{"holdDecision"}},
	{file: "supervisor/wake.go", fn: "Channel.releaseCappedHolds", ordinal: 1, class: classOwnWord, entry: "the cap words",
		set: []string{"hold_reason=NULL"}, where: []string{"hold_reason=?"}, refs: []string{"cappedHolds"}},
	{file: "supervisor/notice_stage.go", fn: "NoticeChannel.StageNotice", ordinal: 1, class: classRule, entry: "restage / readdress (superseded half); hierarchy_unresolved half own-word",
		set: []string{sqlCASETwo, "hold_reason=NULL"}, where: []string{sqlNeverSent}, via: []string{"noticeHoldKept"}, refs: []string{refSuperseded, refUnaddressed}},
	{file: "supervisor/notice_stage.go", fn: "NoticeChannel.Park", ordinal: 1, class: classRule, entry: "notice_park",
		set: []string{"hold_reason=?"}, where: []string{"hold_reason IS NULL", sqlNeverSent}, via: []string{"holdDecision"}, entries: []string{"entryNoticePark"}},
	{file: "supervisor/send.go", fn: "Channel.deferMessage", ordinal: 1, class: classCASFromNone, entry: "defer",
		set: []string{"hold_reason=?"}, where: []string{"hold_reason IS ?"}, via: []string{"refusesDeferral"}, entries: []string{"entryDefer"}},
	{file: "supervisor/deferral.go", fn: "Channel.deferBusy", ordinal: 1, class: classCASFromNone, entry: "defer",
		set: []string{"hold_reason=?"}, where: []string{"hold_reason IS ?"}, via: []string{"refusesDeferral"}, entries: []string{"entryDefer"}},
	{file: "supervisor/send.go", fn: "Channel.holdUnaddressed", ordinal: 1, class: classOwnWord, entry: "hierarchy_unresolved",
		set: []string{"hold_reason='hierarchy_unresolved'"}, where: []string{"hold_reason IS NULL", sqlUnsent}},
	{file: "supervisor/direct.go", fn: "Channel.StageDirect", ordinal: 1, class: classRule, entry: "restage / readdress (superseded half); hierarchy_unresolved half own-word",
		set: []string{sqlCASETwo}, where: []string{sqlNeverSent}, via: []string{"directHoldReleases"}, refs: []string{refSuperseded, refUnaddressed}},
	{file: "store/supervisor_sync_routing.go", fn: "Store.SettleSupervisorMessage", ordinal: 1, class: classStoreHalf, entry: "settle (store half)",
		set: []string{"hold_reason=CASE WHEN hold_reason IS NULL THEN ? ELSE hold_reason END"}},
	{file: "store/supervisor_list.go", fn: "Store.AcknowledgeSupervisorMessages", ordinal: 1, class: classStoreHalf, entry: "acknowledge (store half)",
		set: []string{"hold_reason=?"}, where: []string{"hold_reason IS NULL", "kind<>'decision'", "obligation_kind<>'fault_notification'", sqlNeverSent}},
}

// holdQuietUpdate is an UPDATE supervisor_messages whose SET does not name hold_reason, so it cannot
// change a hold; where is what its WHERE must still hold (the claim's hold_reason IS NULL).
type holdQuietUpdate struct {
	file, fn string
	ordinal  int
	where    []string
}

// holdQuietUpdates are the other UPDATEs of section 3.
var holdQuietUpdates = []holdQuietUpdate{
	{file: "supervisor/proposal.go", fn: "Channel.refreshProposal", ordinal: 1},
	{file: "supervisor/proposal.go", fn: "Channel.refreshEventlessProposal", ordinal: 1},
	{file: "supervisor/wake.go", fn: "Channel.WakeBusyRecipient", ordinal: 1},
	{file: "supervisor/autosend.go", fn: "Channel.deferAutoFault", ordinal: 1},
	{file: "supervisor/readback.go", fn: "Channel.ReadBack", ordinal: 1},
	{file: "supervisor/stage.go", fn: "Channel.StageWithReading", ordinal: 3},
	{file: "supervisor/notice.go", fn: "Channel.refreshNotice", ordinal: 1},
	{file: "supervisor/send.go", fn: "Channel.claim", ordinal: 1, where: []string{"hold_reason IS NULL"}},
	{file: "supervisor/recover.go", fn: "Channel.recoverStranded", ordinal: 1},
	{file: "supervisor/recover.go", fn: "Channel.recoverStranded", ordinal: 2},
}

// holdSupersededWriters are the functions that journal supervisor_message_superseded, each only
// with the set of the hold it records (section 2, invariant 2). The two in the supervisor package ask
// the rule as derive; the store's is the acknowledgement's, guarded by its set from none.
var holdSupersededWriters = map[string][]string{
	"supervisor/send.go#Channel.holdObsoleteClaim":                 {"holdDecision", "deferMessage"},
	"supervisor/proposal.go#Channel.holdSuperseded":                {"holdDecision"},
	"store/supervisor_list.go#Store.AcknowledgeSupervisorMessages": {"changedOne"},
}

// holdSettleCallers are the callers of SettleSupervisorMessage: each asks the store's settle, which
// keeps any hold present (the settle row).
var holdSettleCallers = map[string]bool{
	"supervisor/send.go#Channel.settleWithheld":                 true,
	"supervisor/send.go#attemptRun.settle":                      true,
	"supervisor/proposal.go#Channel.holdSuperseded":             true,
	"supervisor/flow_delivery.go#attemptRun.fenceFlowReportAck": true,
}

// holdWriteStatement is one collected statement or call.
type holdWriteStatement struct {
	kind     string // update, insert, superseded, settle
	file, fn string
	ordinal  int
	pos      string
	variants []string // the normalized SQL, one per value of the local text it is built with
}

func (s holdWriteStatement) key() string { return fmt.Sprintf("%s#%s#%d", s.file, s.fn, s.ordinal) }

// setOf and whereOf are the SET and WHERE clauses of a normalized UPDATE.
func setOf(sql string) string {
	_, rest, _ := strings.Cut(sql, " SET ")
	set, _, _ := strings.Cut(rest, " WHERE ")
	return set
}

func whereOf(sql string) string {
	_, where, _ := strings.Cut(sql, " WHERE ")
	return where
}

var namesHoldReason = regexp.MustCompile(`\bhold_reason=`)

func (s holdWriteStatement) setNamesHold() bool {
	for _, v := range s.variants {
		if namesHoldReason.MatchString(setOf(v)) {
			return true
		}
	}
	return false
}

// holdFunc is what the guard knows of one function: the names it calls, the names it uses, and the
// Entry values of the holdChange literals it passes to holdDecision.
type holdFunc struct {
	calls, refs map[string]bool
	entries     map[string]bool
}

type holdWriteScan struct {
	sites   []holdWriteStatement
	funcs   map[string]*holdFunc // by bare function name; methods of one name share an entry
	byKey   map[string]*holdFunc // by file#Recv.Name
	outside []string             // writes to supervisor_messages found outside the two packages
}

var (
	sqlUpdateMessages   = regexp.MustCompile(`(?i)^\s*UPDATE\s+supervisor_messages\b`)
	sqlInsertMessages   = regexp.MustCompile(`(?i)^\s*INSERT\s+(OR\s+\w+\s+)?INTO\s+supervisor_messages\b`)
	sqlWriteMessages    = regexp.MustCompile(`(?i)\b(UPDATE|INSERT\s+(OR\s+\w+\s+)?INTO|DELETE\s+FROM)\s+supervisor_messages\b`)
	sqlInsertJournal    = regexp.MustCompile(`(?i)^\s*INSERT\s+INTO\s+journal\b`)
	sqlSelect           = regexp.MustCompile(`(?i)^\s*SELECT\b`)
	sqlSpaceAroundEqual = regexp.MustCompile(`\s*(=|<>|,)\s*`)
	sqlSpaces           = regexp.MustCompile(`\s+`)
)

func normalizeSQL(s string) string {
	s = sqlSpaces.ReplaceAllString(s, " ")
	return strings.TrimSpace(sqlSpaceAroundEqual.ReplaceAllString(s, "$1"))
}

// scanHoldWrites parses the two packages under root (the repository) and every other non-test Go
// file under internal/relay for writes to supervisor_messages.
func scanHoldWrites(t *testing.T, root string) holdWriteScan {
	t.Helper()
	scan := holdWriteScan{funcs: map[string]*holdFunc{}, byKey: map[string]*holdFunc{}}
	consts := map[string]string{}
	type parsed struct {
		rel  string
		pkg  string
		file *ast.File
		fset *token.FileSet
	}
	var files []parsed
	relay := filepath.Join(root, "internal", "relay")
	err := filepath.WalkDir(relay, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(relay, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		dir := filepath.ToSlash(filepath.Dir(rel))
		if dir != "supervisor" && dir != "store" {
			ast.Inspect(file, func(n ast.Node) bool {
				if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if text, err := strconv.Unquote(lit.Value); err == nil && sqlWriteMessages.MatchString(text) {
						scan.outside = append(scan.outside, fmt.Sprintf("%s: %s", fset.Position(lit.Pos()), strings.TrimSpace(text)))
					}
				}
				return true
			})
			return nil
		}
		files = append(files, parsed{rel: rel, pkg: dir, file: file, fset: fset})
		// The string constants of both packages, so a statement built with one reads as the word.
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if i < len(vs.Values) {
						if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							if text, err := strconv.Unquote(lit.Value); err == nil {
								consts[dir+"."+name.Name] = text
							}
						}
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ordinals := map[string]int{}
	for _, f := range files {
		for _, decl := range f.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			name := fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) > 0 {
				recv := fn.Recv.List[0].Type
				if star, ok := recv.(*ast.StarExpr); ok {
					recv = star.X
				}
				if id, ok := recv.(*ast.Ident); ok {
					name = id.Name + "." + name
				}
			}
			info := &holdFunc{calls: map[string]bool{}, refs: map[string]bool{}, entries: map[string]bool{}}
			scan.byKey[f.rel+"#"+name] = info
			bare := fn.Name.Name
			if scan.funcs[bare] == nil {
				scan.funcs[bare] = &holdFunc{calls: map[string]bool{}, refs: map[string]bool{}, entries: map[string]bool{}}
			}
			merged := scan.funcs[bare]
			locals := holdLocalTexts(fn.Body, f.pkg, consts)
			render := func(e ast.Expr) []string { return holdRender(e, f.pkg, consts, locals, f.fset) }
			var stack []ast.Node
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if n == nil {
					stack = stack[:len(stack)-1]
					return true
				}
				parent := ast.Node(nil)
				if len(stack) > 0 {
					parent = stack[len(stack)-1]
				}
				stack = append(stack, n)
				switch n := n.(type) {
				case *ast.CallExpr:
					called := ""
					switch fun := n.Fun.(type) {
					case *ast.Ident:
						called = fun.Name
					case *ast.SelectorExpr:
						called = fun.Sel.Name
					}
					if called != "" {
						info.calls[called] = true
						merged.calls[called] = true
					}
					if called == "holdDecision" && len(n.Args) >= 2 {
						if lit, ok := n.Args[1].(*ast.CompositeLit); ok {
							for _, elt := range lit.Elts {
								if kv, ok := elt.(*ast.KeyValueExpr); ok {
									if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Entry" {
										if v, ok := kv.Value.(*ast.Ident); ok {
											info.entries[v.Name] = true
											merged.entries[v.Name] = true
										}
									}
								}
							}
						}
					}
					if called == "SettleSupervisorMessage" {
						key := f.rel + "#" + name + "#settle"
						ordinals[key]++
						scan.sites = append(scan.sites, holdWriteStatement{kind: "settle", file: f.rel, fn: name, ordinal: ordinals[key], pos: f.fset.Position(n.Pos()).String()})
					}
				case *ast.Ident:
					info.refs[n.Name] = true
				case *ast.SelectorExpr:
					if x, ok := n.X.(*ast.Ident); ok {
						info.refs[x.Name+"."+n.Sel.Name] = true
					}
				case *ast.BinaryExpr, *ast.BasicLit:
					// A statement is a string literal, or the outermost + chain that starts with one.
					if b, ok := parent.(*ast.BinaryExpr); ok && b.Op == token.ADD {
						return true
					}
					if b, ok := n.(*ast.BinaryExpr); ok && b.Op != token.ADD {
						return true
					}
					if lit, ok := n.(*ast.BasicLit); ok && lit.Kind != token.STRING {
						return true
					}
					variants := render(n.(ast.Expr))
					if len(variants) == 0 {
						return true
					}
					first := variants[0]
					kind := ""
					switch {
					case sqlUpdateMessages.MatchString(first):
						kind = "update"
					case sqlInsertMessages.MatchString(first):
						kind = "insert"
					case sqlInsertJournal.MatchString(first) && strings.Contains(first, "'supervisor_message_superseded'"):
						kind = "superseded"
					case strings.TrimSpace(first) == "supervisor_message_superseded":
						kind = "superseded"
					case sqlWriteMessages.MatchString(first) && !sqlSelect.MatchString(first):
						kind = "unrecognized"
					}
					if kind == "" {
						return true
					}
					key := f.rel + "#" + name + "#" + kind
					ordinals[key]++
					for i := range variants {
						variants[i] = normalizeSQL(variants[i])
					}
					scan.sites = append(scan.sites, holdWriteStatement{kind: kind, file: f.rel, fn: name, ordinal: ordinals[key], pos: f.fset.Position(n.Pos()).String(), variants: variants})
				}
				return true
			})
		}
	}
	sort.SliceStable(scan.sites, func(i, j int) bool { return scan.sites[i].pos < scan.sites[j].pos })
	return scan
}

// holdLocalTexts are the string values each local variable of body is assigned from literals (and
// constants), so a statement built with one renders once per value.
func holdLocalTexts(body *ast.BlockStmt, pkg string, consts map[string]string) map[string][]string {
	locals := map[string][]string{}
	ast.Inspect(body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || (assign.Tok != token.DEFINE && assign.Tok != token.ASSIGN) || len(assign.Lhs) != len(assign.Rhs) {
			return true
		}
		for i, lhs := range assign.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok {
				continue
			}
			if text, ok := holdConstText(assign.Rhs[i], pkg, consts); ok {
				locals[id.Name] = append(locals[id.Name], text)
			}
		}
		return true
	})
	return locals
}

// holdConstText is the text of a string expression built only from literals and constants.
func holdConstText(e ast.Expr, pkg string, consts map[string]string) (string, bool) {
	switch e := e.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		v := constant.MakeFromLiteral(e.Value, e.Kind, 0)
		return constant.StringVal(v), true
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		a, ok := holdConstText(e.X, pkg, consts)
		if !ok {
			return "", false
		}
		b, ok := holdConstText(e.Y, pkg, consts)
		return a + b, ok
	case *ast.Ident:
		text, ok := consts[pkg+"."+e.Name]
		return text, ok
	case *ast.SelectorExpr:
		if x, ok := e.X.(*ast.Ident); ok {
			text, ok := consts[x.Name+"."+e.Sel.Name]
			return text, ok
		}
	}
	return "", false
}

// holdRender is the text of a + chain, once per combination of the local values it uses: literals
// and constants as their text, a local string variable as each value it is assigned, and anything
// else as {its source}.
func holdRender(e ast.Expr, pkg string, consts map[string]string, locals map[string][]string, fset *token.FileSet) []string {
	if text, ok := holdConstText(e, pkg, consts); ok {
		return []string{text}
	}
	switch e := e.(type) {
	case *ast.BinaryExpr:
		if e.Op == token.ADD {
			var out []string
			for _, a := range holdRender(e.X, pkg, consts, locals, fset) {
				for _, b := range holdRender(e.Y, pkg, consts, locals, fset) {
					out = append(out, a+b)
				}
			}
			return out
		}
	case *ast.Ident:
		if values, ok := locals[e.Name]; ok {
			return values
		}
	}
	var buf bytes.Buffer
	_ = printer.Fprint(&buf, fset, e)
	return []string{"{" + buf.String() + "}"}
}

// holdReaches is whether name, or a function it calls transitively within the two packages, calls
// holdDecision.
func (s holdWriteScan) holdReaches(name string) bool {
	if name == "holdDecision" {
		return true
	}
	seen := map[string]bool{name: true}
	queue := []string{name}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		fn := s.funcs[next]
		if fn == nil {
			continue
		}
		for called := range fn.calls {
			if called == "holdDecision" {
				return true
			}
			if !seen[called] {
				seen[called] = true
				queue = append(queue, called)
			}
		}
	}
	return false
}

// checkHoldWrites is every way scan departs from the section 3 table, one line each.
func checkHoldWrites(scan holdWriteScan) []string {
	var problems []string
	for _, w := range scan.outside {
		problems = append(problems, "a write to supervisor_messages outside internal/relay/supervisor and internal/relay/store: "+w)
	}
	sites := map[string]holdWriteSite{}
	for _, site := range holdWriteSites {
		sites[fmt.Sprintf("%s#%s#%d", site.file, site.fn, site.ordinal)] = site
	}
	quiet := map[string]holdQuietUpdate{}
	for _, q := range holdQuietUpdates {
		quiet[fmt.Sprintf("%s#%s#%d", q.file, q.fn, q.ordinal)] = q
	}
	found := map[string]bool{}
	containsSome := func(parts []string, want string) bool {
		for _, p := range parts {
			if strings.Contains(p, want) {
				return true
			}
		}
		return false
	}
	for _, s := range scan.sites {
		switch s.kind {
		case "unrecognized":
			problems = append(problems, fmt.Sprintf("%s (%s#%s): a write to supervisor_messages the guard does not recognize: %s", s.pos, s.file, s.fn, s.variants[0]))
		case "insert":
			for _, v := range s.variants {
				if strings.Contains(v, "hold_reason") {
					problems = append(problems, fmt.Sprintf("%s (%s#%s): an insert into supervisor_messages names hold_reason; a message is staged with no hold", s.pos, s.file, s.fn))
				}
			}
		case "superseded":
			key := s.file + "#" + s.fn
			needs, ok := holdSupersededWriters[key]
			if !ok {
				problems = append(problems, fmt.Sprintf("%s (%s): journals supervisor_message_superseded outside its writers (holdObsoleteClaim, holdSuperseded, AcknowledgeSupervisorMessages): the newest such row must be the one of the write that set the hold", s.pos, key))
				continue
			}
			found["superseded#"+key] = true
			fn := scan.byKey[key]
			for _, need := range needs {
				if fn == nil || !fn.calls[need] {
					problems = append(problems, fmt.Sprintf("%s (%s): journals supervisor_message_superseded and no longer calls %s", s.pos, key, need))
				}
			}
			if strings.HasPrefix(key, "supervisor/") && (fn == nil || !fn.entries["entryDerive"]) {
				problems = append(problems, fmt.Sprintf("%s (%s): journals supervisor_message_superseded without asking the rule as derive (holdDecision with entryDerive)", s.pos, key))
			}
		case "settle":
			key := s.file + "#" + s.fn
			if !holdSettleCallers[key] {
				problems = append(problems, fmt.Sprintf("%s (%s): calls SettleSupervisorMessage and is not a settle site of the hold table", s.pos, key))
			}
			found["settle#"+key] = true
		case "update":
			key := s.key()
			found[key] = true
			if q, ok := quiet[key]; ok {
				if s.setNamesHold() {
					problems = append(problems, fmt.Sprintf("%s (%s): this UPDATE now names hold_reason in its SET; a hold write must be a site of the hold table and ask the rule", s.pos, key))
				}
				for _, want := range q.where {
					if !containsSome(wheresOf(s.variants), want) {
						problems = append(problems, fmt.Sprintf("%s (%s): its WHERE no longer holds %q", s.pos, key, want))
					}
				}
				continue
			}
			site, ok := sites[key]
			if !ok {
				if s.setNamesHold() {
					problems = append(problems, fmt.Sprintf("%s (%s): an UPDATE supervisor_messages that sets hold_reason and is not a site of the hold table (plan-r3 section 3): %s", s.pos, key, s.variants[0]))
				} else {
					problems = append(problems, fmt.Sprintf("%s (%s): an UPDATE supervisor_messages the hold table does not list (add it to holdQuietUpdates when it cannot change a hold): %s", s.pos, key, s.variants[0]))
				}
				continue
			}
			if !s.setNamesHold() {
				problems = append(problems, fmt.Sprintf("%s (%s): a site of the hold table whose SET no longer names hold_reason", s.pos, key))
			}
			for _, want := range site.set {
				if !containsSome(setsOf(s.variants), want) {
					problems = append(problems, fmt.Sprintf("%s (%s, %s %s): its SET does not hold %q", s.pos, key, site.class, site.entry, want))
				}
			}
			for _, want := range site.where {
				if !containsSome(wheresOf(s.variants), want) {
					problems = append(problems, fmt.Sprintf("%s (%s, %s %s): its WHERE does not hold %q", s.pos, key, site.class, site.entry, want))
				}
			}
			fn := scan.byKey[s.file+"#"+s.fn]
			for i, via := range site.via {
				if fn == nil || !fn.calls[via] {
					problems = append(problems, fmt.Sprintf("%s (%s, %s %s): its function no longer calls %s", s.pos, key, site.class, site.entry, via))
					continue
				}
				if !scan.holdReaches(via) {
					problems = append(problems, fmt.Sprintf("%s (%s, %s %s): %s no longer reaches holdDecision", s.pos, key, site.class, site.entry, via))
				}
				if i == 0 && len(site.entries) > 0 {
					asker := fn
					if via != "holdDecision" {
						asker = scan.funcs[via]
					}
					for _, entry := range site.entries {
						if asker == nil || !asker.entries[entry] {
							problems = append(problems, fmt.Sprintf("%s (%s, %s %s): %s does not ask holdDecision with %s", s.pos, key, site.class, site.entry, via, entry))
						}
					}
				}
			}
			for _, ref := range site.refs {
				if fn == nil || !fn.refs[ref] {
					problems = append(problems, fmt.Sprintf("%s (%s, %s %s): its function no longer names %s", s.pos, key, site.class, site.entry, ref))
				}
			}
		}
	}
	for key, site := range sites {
		if !found[key] {
			problems = append(problems, fmt.Sprintf("%s (%s %s): a site of the hold table the code no longer has; update the table with the code", key, site.class, site.entry))
		}
	}
	for key := range quiet {
		if !found[key] {
			problems = append(problems, fmt.Sprintf("%s: a quiet UPDATE of the hold table the code no longer has", key))
		}
	}
	for key := range holdSupersededWriters {
		if !found["superseded#"+key] {
			problems = append(problems, fmt.Sprintf("%s: a supervisor_message_superseded writer the code no longer has", key))
		}
	}
	for key := range holdSettleCallers {
		if !found["settle#"+key] {
			problems = append(problems, fmt.Sprintf("%s: a SettleSupervisorMessage caller the code no longer has", key))
		}
	}
	sort.Strings(problems)
	return problems
}

func setsOf(variants []string) []string {
	out := make([]string, len(variants))
	for i, v := range variants {
		out[i] = setOf(v)
	}
	return out
}

func wheresOf(variants []string) []string {
	out := make([]string, len(variants))
	for i, v := range variants {
		out[i] = whereOf(v)
	}
	return out
}

// TestHoldObsoleteClaimJournalsOnlyItsOwnSet (CRW-1500 U10, invariant 2 of plan-r3 section 2):
// holdObsoleteClaim journals supervisor_message_superseded only with the derived hold it sets. Given
// a row that already carries the acknowledgement's park (its CAS from the read row then defers
// nothing), it writes no superseded row, so the newest such row is still the acknowledgement's and
// the park still reads as acknowledged rather than as a derived hold an owed restage would release.
func TestHoldObsoleteClaimJournalsOnlyItsOwnSet(t *testing.T) {
	w := newHoldWorld(t, "event")
	w.setHold(t, "acknowledged")
	row, err := w.c.Get(w.ctx, w.id)
	if err != nil {
		t.Fatal(err)
	}
	before := w.journalRows(t, "supervisor_message_superseded")
	if err := w.c.holdObsoleteClaim(w.ctx, row, w.c.nowISO(), "obsolete at the claim"); err != nil {
		t.Fatal(err)
	}
	if after := w.journalRows(t, "supervisor_message_superseded"); after != before {
		t.Fatalf("holdObsoleteClaim on a parked row journaled %d superseded row(s), want none", after-before)
	}
	w.expectParked(t, holdAcknowledged, "queued", 0)
}
