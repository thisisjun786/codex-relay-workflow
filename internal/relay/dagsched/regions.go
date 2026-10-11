package dagsched

import (
	"context"
	"database/sql"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Region is one place in a repository a node expects to edit (contract 7.2). Kind is tree, file or symbol; Change is edit, rename or delete.
// Exclusive is the declarer's word that the node holds the whole repository (a repository-wide rename, say): the region conflicts with every other region of its repository. Nothing else sets it (CRW-431): a rename, a
// delete and a hotspot file are exclusive at their own place and nowhere else (Classify, foldedGrade).
// Grade says how an overlap on the place is settled and Rule, for a mechanical grade, how (CRW-409, grades.go); a region declared without a grade is independent.
type Region struct {
	Repository, Path, Kind, Key, Change string
	Exclusive                           bool
	Grade, Rule                         string
}

var hotspotNames = map[string]bool{
	"go.mod": true, "go.sum": true, "package-lock.json": true, "yarn.lock": true, "pnpm-lock.yaml": true, "Cargo.lock": true,
	"poetry.lock": true, "Pipfile.lock": true, "uv.lock": true, "Makefile": true, "Dockerfile": true,
}

// Classify says whether a change to a place is exclusive at that place, and why: a rename or a delete, or a hotspot (a lockfile, anything under .github/, a Makefile or a
// Dockerfile, a .sql file, a path with a schema or migrations segment), is the kind of change two parallel branches cannot both make. The hold is on the place and not on the repository (CRW-431): the
// region is judged exclusive against whatever shares its place (the same path, a tree that covers it, what lies under a tree it deletes or renames) and against nothing else; a whole-repository hold is
// only what the declarer states (Region.Exclusive).
func Classify(p, change string) (exclusive bool, why string) {
	if change == "rename" || change == "delete" {
		return true, change
	}
	clean := strings.TrimPrefix(path.Clean(p), "/")
	if hotspotNames[path.Base(clean)] {
		return true, "hotspot"
	}
	if clean == ".github" || strings.HasPrefix(clean, ".github/") || strings.HasSuffix(clean, ".sql") {
		return true, "hotspot"
	}
	for _, segment := range strings.Split(clean, "/") {
		if segment == "schema" || segment == "migrations" {
			return true, "hotspot"
		}
	}
	return false, ""
}

// placeHold is whether Classify makes the region exclusive at its place.
func placeHold(r Region) bool {
	exclusive, _ := Classify(r.Path, r.Change)
	return exclusive
}

func within(p, dir string) bool { return p == dir || strings.HasPrefix(p, dir+"/") }

// Overlaps is whether two regions can be touched by the same change. Different repositories never overlap; a region whose declarer stated a whole-repository hold overlaps everything in its repository; a tree
// overlaps whatever lies under it; two symbols overlap only when they are the same symbol, unless one of them is a delete, a rename, a hotspot or a shared contract file, which makes the file one place (wholeFile);
// any other pair on one path overlaps. A delete, a rename and a hotspot overlap only what shares their place: they hold no more of the repository than the same path and the trees that cover it. It says nothing
// of how the overlap is settled: PairGrade does.
func Overlaps(a, b Region) bool {
	if a.Repository != b.Repository {
		return false
	}
	if a.Exclusive || b.Exclusive {
		return true
	}
	_, _, ok := commonPlace(a, b)
	return ok
}

// loadDeclarations is the latest declaration of every node of a plan (the rows with the highest declaration_seq per node). A node absent from the map has none: its
// regions are unknown. The grade of a region is read from dag_node_region_grades (a row of a declaration made before grades existed has none and reads as independent) and folded the
// way a declaration is (foldedGrade), so a stored declaration compares equal to the same one declared now. A store whose zone predates that table is read without it.
//
// Whether the region holds the whole repository is read from dag_node_region_holds, which a declaration made since CRW-431 fills for every region with what the declarer stated. A declaration made before
// has no row and carries the exclusive column the classifier set (a rename, a delete or a hotspot) or the caller's word; it reads as follows: a delete and a hotspot protected nothing outside their own place and read at
// it; a rename protected a destination no region can name, and a region the classifier does not flag can only have been held by the declarer, so those two stay a hold of the repository until the node is
// declared again. A store whose zone predates dag_node_region_holds reads every row that way.
func loadDeclarations(ctx context.Context, q store.Querier, plan string) (map[string][]Region, error) {
	graded, err := tableExists(ctx, q, "dag_node_region_grades")
	if err != nil {
		return nil, err
	}
	columns, join := ", '', ''", ""
	if graded {
		columns = ", COALESCE(g.grade, ''), COALESCE(g.rule, '')"
		join = " LEFT JOIN dag_node_region_grades g ON g.plan_id = r.plan_id AND g.node_id = r.node_id AND g.declaration_seq = r.declaration_seq AND g.repository = r.repository" +
			" AND g.path = r.path AND g.region_kind = r.region_kind AND g.region_key = r.region_key"
	}
	held, err := tableExists(ctx, q, "dag_node_region_holds")
	if err != nil {
		return nil, err
	}
	stated := ", -1"
	if held {
		stated = ", COALESCE(h.stated, -1)"
		join += " LEFT JOIN dag_node_region_holds h ON h.plan_id = r.plan_id AND h.node_id = r.node_id AND h.declaration_seq = r.declaration_seq AND h.repository = r.repository" +
			" AND h.path = r.path AND h.region_kind = r.region_kind AND h.region_key = r.region_key"
	}
	rows, err := q.QueryContext(ctx, "SELECT r.node_id, r.repository, r.path, r.region_kind, r.region_key, r.change, r.exclusive"+columns+stated+" FROM dag_node_regions r"+join+
		" WHERE r.plan_id = ? AND r.declaration_seq = (SELECT MAX(declaration_seq) FROM dag_node_regions m WHERE m.plan_id = r.plan_id AND m.node_id = r.node_id)"+
		" ORDER BY r.node_id, r.repository, r.path, r.region_kind, r.region_key", plan)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]Region{}
	for rows.Next() {
		var node string
		var r Region
		var exclusive, stated int
		if err := rows.Scan(&node, &r.Repository, &r.Path, &r.Kind, &r.Key, &r.Change, &exclusive, &r.Grade, &r.Rule, &stated); err != nil {
			return nil, err
		}
		if stated >= 0 {
			r.Exclusive = stated == 1
		} else {
			r.Exclusive = exclusive == 1 && (r.Change == "rename" || !placeHold(r))
		}
		r.Grade, r.Rule = foldedGrade(r)
		out[node] = append(out[node], r)
	}
	return out, rows.Err()
}

// A holder is a node whose edit regions are in use or claimed in this pass: its declaration, or none (Unknown).
type holder struct {
	NodeID  string
	Regions []Region
	Unknown bool
}

// MaxRegions bounds one declaration.
const MaxRegions = 64

// RegionDeclaration is the answer of DeclareRegions: the sequence number of the declaration now in force and the regions it holds (as stored: sorted,
// with the exclusive flag the classifier and the caller agreed on).
type RegionDeclaration struct {
	PlanID, NodeID string
	Seq            int64
	Replayed       bool
	// Narrowed is set when the node already held its regions and the declaration replaced them with a narrowing of them (CRW-411).
	Narrowed bool
	Regions  []Region
}

// DeclareRegions records the edit regions of an implementation node before it is released (contract 7.2). A declaration replaces the node's earlier one
// as a whole; one identical to the latest is a replay and writes nothing. The classifier makes a rename, a delete and the hotspots exclusive at their own place whatever grade the caller
// said (foldedGrade); a hold of the whole repository is only what the caller states with Region.Exclusive. A grade is stored with every region: a mechanical one with its rule, and a shared contract surface as exclusive
// whatever the caller said (grades.go). What the caller stated is stored with every region too (dag_node_region_holds), so a declaration made by this build reads back as it was made and one made before reads as
// loadDeclarations says; the exclusive column of dag_node_regions keeps the value an older runtime reads as a hold (stated, or flagged by the classifier).
//
// Regions are held from release until the head lands (contract 7.2), so a node that holds them may declare again only to narrow them (narrowing.go): every new region inside a held one and
// held at least as strictly, which frees the rest for the next pass. A declaration that widens them is refused disposition_conflict, naming the region. A node whose head is accepted keeps
// what it holds, and one that never declared has nothing to narrow.
func (s *Scheduler) DeclareRegions(ctx context.Context, plan, node, actor string, regions []Region) (RegionDeclaration, error) {
	normal, err := normalizeRegions(regions)
	if err != nil {
		return RegionDeclaration{}, err
	}
	out := RegionDeclaration{PlanID: plan, NodeID: node, Regions: normal}
	err = s.Store.Transaction(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		q := s.Store.Q(txCtx)
		if err := s.fence(txCtx, q, plan, actor); err != nil {
			return err
		}
		snap, n, err := liveNode(txCtx, q, plan, node)
		if err != nil {
			return err
		}
		if n.Kind != dag.NodeImplementation {
			return refuse(contract.RefusalDispositionConflict, "node %s is a %s node: it edits no repository, so it has no regions to declare", node, n.Kind)
		}
		current, err := loadDeclarations(txCtx, q, plan)
		if err != nil {
			return err
		}
		latest, declared := current[node]
		replay := declared && slices.Equal(latest, normal)
		if !replay {
			// regions are held until the node lands (contract 7.2): once a release or an execution holds them a different declaration may only narrow them.
			state, err := s.stateOf(txCtx, q, plan, snap, n)
			if err != nil {
				return err
			}
			if state.Holds {
				switch {
				case state.HasAcc:
					return refuse(contract.RefusalDispositionConflict, "node %s is %s and its accepted head holds its regions until it lands: the declaration describes the pull request, so it is not narrowed", node, state.State)
				case !declared:
					return refuse(contract.RefusalDispositionConflict, "node %s is %s and holds regions it never declared, so there is nothing to narrow; a declaration is made before the release", node, state.State)
				}
				if why := widening(latest, normal); why != "" {
					return refuse(contract.RefusalDispositionConflict, "node %s is %s and holds its regions until its head lands; a declaration may only narrow them: %s", node, state.State, why)
				}
				out.Narrowed = true
			}
		}
		if replay {
			var seq sql.NullInt64
			if err := q.QueryRowContext(txCtx, "SELECT MAX(declaration_seq) FROM dag_node_regions WHERE plan_id = ? AND node_id = ?", plan, node).Scan(&seq); err != nil {
				return err
			}
			out.Seq, out.Replayed = seq.Int64, true
			return nil
		}
		var last sql.NullInt64
		if err := q.QueryRowContext(txCtx, "SELECT MAX(declaration_seq) FROM dag_node_regions WHERE plan_id = ? AND node_id = ?", plan, node).Scan(&last); err != nil {
			return err
		}
		out.Seq = last.Int64 + 1
		at := s.now()
		for _, r := range normal {
			exclusive, stated := 0, 0
			if r.Exclusive {
				stated = 1
			}
			if r.Exclusive || placeHold(r) {
				exclusive = 1
			}
			if _, err := q.ExecContext(txCtx, "INSERT INTO dag_node_regions (plan_id, node_id, declaration_seq, repository, path, region_kind, region_key, change, exclusive, declared_by, declared_at)"+
				" VALUES (?,?,?,?,?,?,?,?,?,?,?)", plan, node, out.Seq, r.Repository, r.Path, r.Kind, r.Key, r.Change, exclusive, actor, at); err != nil {
				return err
			}
			if _, err := q.ExecContext(txCtx, "INSERT INTO dag_node_region_grades (plan_id, node_id, declaration_seq, repository, path, region_kind, region_key, grade, rule) VALUES (?,?,?,?,?,?,?,?,?)",
				plan, node, out.Seq, r.Repository, r.Path, r.Kind, r.Key, r.Grade, r.Rule); err != nil {
				return err
			}
			if _, err := q.ExecContext(txCtx, "INSERT INTO dag_node_region_holds (plan_id, node_id, declaration_seq, repository, path, region_kind, region_key, stated) VALUES (?,?,?,?,?,?,?,?)",
				plan, node, out.Seq, r.Repository, r.Path, r.Kind, r.Key, stated); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// normalizeRegions validates and canonicalises a declaration: 1 to 64 distinct regions, each with a repository, a relative clean path, a kind, a change
// and, for a symbol, its key, and a grade (a mechanical one names its rule). The result is sorted so two spellings of one declaration compare equal.
func normalizeRegions(in []Region) ([]Region, error) {
	if len(in) == 0 || len(in) > MaxRegions {
		return nil, refuse(contract.RefusalMalformedReceipt, "a declaration holds 1 to %d regions, not %d", MaxRegions, len(in))
	}
	type key struct{ repository, path, kind, key string }
	seen := map[key]Region{}
	out := make([]Region, 0, len(in))
	for _, r := range in {
		if r.Change == "" {
			r.Change = "edit"
		}
		repository, err := canonicalRepository(r.Repository)
		if err != nil {
			return nil, err
		}
		r.Repository = repository
		// A path is kept as written, spaces inside a name included: trimming would store another place than the one declared. Whitespace around the whole
		// path is refused rather than guessed at.
		if r.Path != strings.TrimSpace(r.Path) || dag.HasControl(r.Path) {
			return nil, refuse(contract.RefusalMalformedReceipt, "region path %q has whitespace or a control character at its ends or inside it", r.Path)
		}
		clean := path.Clean(r.Path)
		switch {
		case clean == "." || strings.HasPrefix(clean, "/") || clean == ".." || strings.HasPrefix(clean, "../") || strings.ContainsRune(clean, 0):
			return nil, refuse(contract.RefusalMalformedReceipt, "region path %q is not a clean path inside the repository", r.Path)
		case r.Kind != "tree" && r.Kind != "file" && r.Kind != "symbol":
			return nil, refuse(contract.RefusalMalformedReceipt, "region kind %q is tree, file or symbol", r.Kind)
		case r.Change != "edit" && r.Change != "rename" && r.Change != "delete":
			return nil, refuse(contract.RefusalMalformedReceipt, "region change %q is edit, rename or delete", r.Change)
		case r.Kind == "symbol" && r.Key == "":
			return nil, refuse(contract.RefusalMalformedReceipt, "a symbol region names its symbol")
		case r.Kind != "symbol" && r.Key != "":
			return nil, refuse(contract.RefusalMalformedReceipt, "only a symbol region has a key")
		}
		r.Path = clean
		if err := checkGrade(r); err != nil {
			return nil, err
		}
		r.Grade, r.Rule = foldedGrade(r)
		k := key{r.Repository, r.Path, r.Kind, r.Key}
		if earlier, dup := seen[k]; dup {
			if earlier != r {
				return nil, refuse(contract.RefusalMalformedReceipt, "region %s %s is declared twice with different changes or grades", r.Repository, r.Path)
			}
			continue
		}
		seen[k] = r
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch {
		case a.Repository != b.Repository:
			return a.Repository < b.Repository
		case a.Path != b.Path:
			return a.Path < b.Path
		case a.Kind != b.Kind:
			return a.Kind < b.Kind
		}
		return a.Key < b.Key
	})
	return out, nil
}

// slugPattern is a forge repository, owner/name.
var slugPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// canonicalRepository is the one spelling of a repository a lock is keyed by: a local checkout is the directory its absolute path reaches, symbolic links resolved
// (so /repo, /repo/, /repo/. and a link to it are one repository; a path that does not resolve to an existing directory is refused), a forge repository is exactly owner/name. Anything else (a relative
// path among it) and any whitespace around or control character inside is refused: two spellings of one repository would be two locks. A forge slug and a
// checkout path of the same repository remain two keys; a plan names one spelling per repository, the target_repository of its edges.
func canonicalRepository(in string) (string, error) {
	switch {
	case in == "" || in != strings.TrimSpace(in) || dag.HasControl(in):
		return "", refuse(contract.RefusalMalformedReceipt, "region repository %q is empty or has whitespace around it or a control character in it", in)
	case strings.HasPrefix(in, "/"):
		// The identity of a checkout is the directory the path reaches: links are resolved with the path as written (a ".." after a link means the link
		// target's parent, which cleaning first would get wrong). A path that cannot be resolved has no identity to lock under and is refused, so the same
		// string can never be stored as two keys because a link appeared between two declarations.
		resolved, err := filepath.EvalSymlinks(in)
		if err != nil {
			return "", refuse(contract.RefusalMalformedReceipt, "region repository %q cannot be resolved to a directory: %v", in, err)
		}
		if info, err := os.Stat(resolved); err != nil || !info.IsDir() {
			return "", refuse(contract.RefusalMalformedReceipt, "region repository %q is not a directory", in)
		}
		return resolved, nil
	}
	owner, name, _ := strings.Cut(in, "/")
	if !slugPattern.MatchString(in) || owner == "." || owner == ".." || name == "." || name == ".." {
		return "", refuse(contract.RefusalMalformedReceipt, "region repository %q is neither an absolute path nor owner/name", in)
	}
	return in, nil
}

// The owner of a shared place (R04, CRW-1212; the reproduction cases d4 and P3 of the cancelled CRW-1003). Agent nodes of one issue run side by side with no managed start between them, so nothing
// orders them the way a relationship's one-live-owner rule does, and the declarations decide: a place that several of them take has exactly one owner, the node whose declaration holds the place
// at the exclusive grade (foldedGrade: the declarer's word, a shared contract surface, a delete, a rename or a hotspot at its own place). Nothing is judged for a node without a declaration (its regions
// are unknown) or for nodes of another issue.
//
// The judgement is per PLACE, not per pair of nodes. Two nodes that edit one file under a directory another node holds exclusive have an owner, the node that holds the directory, although neither of
// the pair does: a broad owner recorded by a tree is looked for among every declaration of the issue, and it counts for every file, symbol and directory under it. The key of a place is the repository, the
// path and the KIND of place as a suffix, so a whole-repository hold (path "*"), a directory and a file are three places even when their paths read alike.
const (
	// RegionOwnerMissing and RegionOwnerAmbiguous are the closed reasons of the refusal, carried in the detail of a disposition_conflict (D-02: no new refusal reason).
	RegionOwnerMissing   = "agent_region_owner_missing"
	RegionOwnerAmbiguous = "agent_region_owner_ambiguous"
)

// The kinds of place, the suffix of a place key.
const (
	placeRepo   = "repo"
	placeTree   = "tree"
	placeFile   = "file"
	placeSymbol = "symbol"
)

// regionPlace is a place two regions share.
type regionPlace struct{ Repository, Path, Kind, Symbol string }

// key is the place's key: repository, path and kind joined by NUL, and the symbol after the kind for a symbol place.
func (p regionPlace) key() string {
	key := p.Repository + "\x00" + p.Path + "\x00" + p.Kind
	if p.Kind == placeSymbol {
		key += "\x00" + p.Symbol
	}
	return key
}

// label is the place as a refusal names it.
func (p regionPlace) label() string {
	switch p.Kind {
	case placeRepo:
		return "the whole of " + p.Repository
	case placeSymbol:
		return p.Kind + " " + p.Symbol + " of " + p.Path + " in " + p.Repository
	}
	return p.Kind + " " + p.Path + " in " + p.Repository
}

// placeOfRegion is the place a region takes by itself.
func placeOfRegion(r Region) regionPlace {
	p := regionPlace{Repository: r.Repository, Path: path.Clean(r.Path), Kind: r.Kind}
	if r.Kind == "symbol" {
		p.Symbol = r.Key
	}
	return p
}

// sharedPlace is the place two overlapping regions share: the whole repository when both hold it, the other region's place when one does, else the place the place rules agree on (commonPlace).
func sharedPlace(a, b Region) (regionPlace, bool) {
	if a.Repository != b.Repository {
		return regionPlace{}, false
	}
	switch {
	case a.Exclusive && b.Exclusive:
		return regionPlace{Repository: a.Repository, Path: "*", Kind: placeRepo}, true
	case a.Exclusive:
		return placeOfRegion(b), true
	case b.Exclusive:
		return placeOfRegion(a), true
	}
	shared, tree, ok := commonPlace(a, b)
	if !ok {
		return regionPlace{}, false
	}
	switch {
	case tree:
		return regionPlace{Repository: a.Repository, Path: shared, Kind: placeTree}, true
	case a.Kind == "symbol" && b.Kind == "symbol" && !wholeFile(a) && !wholeFile(b):
		return regionPlace{Repository: a.Repository, Path: shared, Kind: placeSymbol, Symbol: a.Key}, true
	}
	return regionPlace{Repository: a.Repository, Path: shared, Kind: placeFile}, true
}

// regionCovers is whether a region takes the whole of a place: a stated whole-repository hold covers every place of its repository, a tree every place under it, a file its own path (and the symbols of
// it), and a symbol its own symbol, or the whole file when it makes the file one place.
func regionCovers(r Region, p regionPlace) bool {
	if r.Repository != p.Repository {
		return false
	}
	if r.Exclusive {
		return true
	}
	rp := path.Clean(r.Path)
	switch p.Kind {
	case placeRepo:
		return false
	case placeTree:
		return r.Kind == "tree" && within(p.Path, rp)
	case placeFile:
		return r.Kind == "tree" && within(p.Path, rp) || r.Kind == "file" && rp == p.Path || r.Kind == "symbol" && rp == p.Path && wholeFile(r)
	}
	return r.Kind == "tree" && within(p.Path, rp) || r.Kind == "file" && rp == p.Path || r.Kind == "symbol" && rp == p.Path && (r.Key == p.Symbol || wholeFile(r))
}

// regionOwnerFinding is a place of the node's declaration that other nodes of its issue take as well and that does not have exactly one owner.
type regionOwnerFinding struct {
	Place regionPlace
	// Takers are the nodes whose declarations overlap on the place (the node itself among them); Owners the nodes that hold it at the exclusive grade, wherever the declaration records it.
	Takers, Owners []string
	Reason         string // RegionOwnerMissing or RegionOwnerAmbiguous
}

// regionOwnerFindings judges every place the node shares with a sibling. siblings are the declarations of the other nodes of the issue.
func regionOwnerFindings(node string, mine []Region, siblings map[string][]Region) []regionOwnerFinding {
	places := map[string]regionPlace{}
	takers := map[string]map[string]bool{}
	ids := make([]string, 0, len(siblings))
	for id := range siblings {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, m := range mine {
		for _, id := range ids {
			for _, o := range siblings[id] {
				p, ok := sharedPlace(m, o)
				if !ok {
					continue
				}
				key := p.key()
				places[key] = p
				if takers[key] == nil {
					takers[key] = map[string]bool{}
				}
				takers[key][node], takers[key][id] = true, true
			}
		}
	}
	keys := make([]string, 0, len(places))
	for key := range places {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	declarations := map[string][]Region{node: mine}
	for id, regions := range siblings {
		declarations[id] = regions
	}
	var out []regionOwnerFinding
	for _, key := range keys {
		place := places[key]
		var owners []string
		for id, regions := range declarations {
			for _, r := range regions {
				if EffectiveGrade(r) == GradeExclusive && regionCovers(r, place) {
					owners = append(owners, id)
					break
				}
			}
		}
		sort.Strings(owners)
		finding := regionOwnerFinding{Place: place, Owners: owners}
		for id := range takers[key] {
			finding.Takers = append(finding.Takers, id)
		}
		sort.Strings(finding.Takers)
		switch {
		case len(owners) == 0:
			finding.Reason = RegionOwnerMissing
		case len(owners) > 1:
			finding.Reason = RegionOwnerAmbiguous
		default:
			continue
		}
		out = append(out, finding)
	}
	return out
}

// agentRegionOwners is the region-owner check of an agent release: the declarations of the node and of the other live implementation nodes of its issue are read from the store and judged by
// regionOwnerFindings. A node that is not an implementation node, or declared nothing, is not judged.
func agentRegionOwners(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, n dag.SnapNode) error {
	if n.Kind != dag.NodeImplementation {
		return nil
	}
	declared, err := loadDeclarations(ctx, q, plan)
	if err != nil {
		return err
	}
	mine, ok := declared[n.NodeID]
	if !ok || len(mine) == 0 {
		return nil
	}
	siblings := map[string][]Region{}
	for _, other := range snap.Nodes {
		if other.NodeID == n.NodeID || other.Kind != dag.NodeImplementation || other.Lifecycle != "" || other.IssueKey != n.IssueKey {
			continue
		}
		if theirs, ok := declared[other.NodeID]; ok && len(theirs) > 0 {
			siblings[other.NodeID] = theirs
		}
	}
	findings := regionOwnerFindings(n.NodeID, mine, siblings)
	if len(findings) == 0 {
		return nil
	}
	f := findings[0]
	switch f.Reason {
	case RegionOwnerAmbiguous:
		return refuse(contract.RefusalDispositionConflict, "%s: nodes %s of issue %s all take %s and %s hold it at the exclusive grade: a place has one owner, so one keeps the grade and the others take a grade that settles the overlap",
			f.Reason, strings.Join(f.Takers, ", "), n.IssueKey, f.Place.label(), strings.Join(f.Owners, ", "))
	}
	return refuse(contract.RefusalDispositionConflict, "%s: nodes %s of issue %s all take %s and none holds it at the exclusive grade: a place several nodes of one issue take needs exactly one owner, declared with the exclusive grade (a directory that covers it counts)",
		f.Reason, strings.Join(f.Takers, ", "), n.IssueKey, f.Place.label())
}
