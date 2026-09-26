//go:build dev

package ci

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Port of scripts/check_operations_contract.py: the operations fixtures replayed against the
// contract they claim to follow. Citation and shape only; see that script's docstring.
var (
	opsClause      = regexp.MustCompile(`OPS-\d+(?:\.\d+)?`)
	opsHeading     = regexp.MustCompile(`^#{2,3}\s+(OPS-\d+(?:\.\d+)?)`)
	opsNormative   = regexp.MustCompile(`^###\s+(OPS-\d+\.\d+)`)
	opsRegisterRow = regexp.MustCompile(`^\|\s*(OPS-\d+\.\d+)\s*\|`)
	opsTicked      = regexp.MustCompile("`([A-Za-z][A-Za-z0-9_]*)`")
	opsScenario    = regexp.MustCompile(`^##\s+(S\d+)\s+(.+)$`)
	opsParts       = []string{"Observed:", "Clauses:", "Action:", "Preserved:"}
	opsMinimum     = map[string]int{"Observed:": 40, "Clauses:": 7, "Action:": 80, "Preserved:": 40}
	opsFieldValues = []string{"verified", "not_verified", "unknown", "not_applicable"}
	opsPointKeys   = []string{"interpreter", "codexCli", "appServer", "host", "date", "measuredBy", "method"}
)

// init makes the ASCII-only patterns above match any Unicode decimal digit, as Python's \d
// does for str patterns.
func init() {
	digit := `[\p{Nd}]`
	for _, p := range []**regexp.Regexp{&opsClause, &opsHeading, &opsNormative, &opsRegisterRow, &opsScenario} {
		*p = regexp.MustCompile(strings.ReplaceAll((*p).String(), `\d`, digit))
	}
	space := `[` + pySpace + `]`
	for _, p := range []**regexp.Regexp{&opsHeading, &opsNormative, &opsRegisterRow, &opsScenario} {
		*p = regexp.MustCompile(strings.ReplaceAll((*p).String(), `\s`, space))
	}
}

func contractClauses(text string) (map[string]bool, map[string]bool) {
	defined, normative := map[string]bool{}, map[string]bool{}
	for _, line := range pySplitlines(text) {
		if m := opsHeading.FindStringSubmatch(line); m != nil {
			if pyWordBoundaryAfter(line, m[0]) {
				defined[m[1]] = true
			} else if base, _, dotted := strings.Cut(m[1], "."); dotted {
				// Python backtracks the optional .digits when the trailing boundary fails.
				prefix := strings.TrimSuffix(m[0], m[1]) + base
				if pyWordBoundaryAfter(line, prefix) {
					defined[base] = true
				}
			}
		}
		if m := opsNormative.FindStringSubmatch(line); m != nil && pyWordBoundaryAfter(line, m[0]) {
			normative[m[1]] = true
		}
		if m := opsRegisterRow.FindStringSubmatch(line); m != nil {
			defined[m[1]] = true
		}
	}
	for clause := range defined {
		defined[strings.Split(clause, ".")[0]] = true
	}
	return defined, normative
}

// pyWordBoundaryAfter enforces Python's Unicode trailing \b after the regex match
// (Go's \b is ASCII-only and rejects matches ending in a Unicode digit).
func pyWordBoundaryAfter(line, match string) bool {
	rest := []rune(line[len(match):])
	return len(rest) == 0 || !isPyWord(rest[0])
}

func isPyWord(r rune) bool {
	return r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
		(r > 0x7f && (isLetterOrDigit(r)))
}

func declaredCheckFields(contract string) map[string]bool {
	names := map[string]bool{}
	inside := false
	for _, line := range pySplitlines(contract) {
		if strings.HasPrefix(line, "### OPS-6.1") {
			inside = true
			continue
		}
		if inside && strings.HasPrefix(line, "###") {
			break
		}
		if inside && strings.HasPrefix(line, "| ") && !strings.HasPrefix(line, "| Field") && !strings.Contains(line, "---") {
			if found := opsTicked.FindStringSubmatch(strings.Split(line, "|")[1]); found != nil {
				names[found[1]] = true
			}
		}
	}
	return names
}

// dictItems is isinstance(value, dict). Where check_operations_contract.py would raise on a non-object (an
// AttributeError traceback), the Go check reports the malformed record and exits 1 instead.
func dictItems(value any) (*pyDict, bool) {
	d, ok := asDict(value)
	return d, ok
}

func checkResultRecord(contract string, record *pyDict, problems []string) ([]string, error) {
	declared := declaredCheckFields(contract)
	fields := &pyDict{vals: map[string]any{}}
	if record.has("fields") {
		d, ok := dictItems(record.get("fields"))
		if !ok {
			return nil, fmt.Errorf("check-result fields is not an object")
		}
		fields = d
	}
	present := map[string]bool{}
	for _, key := range fields.keys {
		present[key] = true
	}
	if len(declared) == 0 {
		problems = append(problems, "OPS-6.1 declares no fields, so the check-result example cannot be verified")
	} else if !sameSet(declared, present) {
		problems = append(problems, "check-result fields "+pyReprList(sortedKeys(present))+" do not match OPS-6.1 "+pyReprList(sortedKeys(declared)))
	}
	for _, name := range fields.keys {
		field, ok := dictItems(fields.get(name))
		if !ok {
			return nil, fmt.Errorf("check-result field %s is not an object", name)
		}
		for _, key := range []string{"value", "evidence", "command", "actor", "measuredAt"} {
			if !field.has(key) {
				problems = append(problems, "check-result field "+name+" is missing "+key)
			}
		}
		value, isString := field.get("value").(string)
		if !isString || !contains(opsFieldValues, value) {
			problems = append(problems, "check-result field "+name+" has an undeclared value")
		}
	}
	return problems, nil
}

func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for key := range a {
		if !b[key] {
			return false
		}
	}
	return true
}

func contains(items []string, item string) bool {
	for _, x := range items {
		if x == item {
			return true
		}
	}
	return false
}

// pyTruthy is bool(value) for a decoded JSON value.
func pyTruthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case []any:
		return len(v) > 0
	case *pyDict:
		return len(v.keys) > 0
	}
	if f, nan, ok := pyNumber(value); ok {
		return nan || f.Sign() != 0
	}
	return true
}

func compatibilityRecord(record *pyDict, problems []string) ([]string, error) {
	var components []any
	if record.has("components") {
		list, ok := record.get("components").([]any)
		if !ok {
			return nil, fmt.Errorf("compatibility components is not a list")
		}
		components = list
	}
	if len(components) == 0 {
		problems = append(problems, "the compatibility example records no component")
	}
	shapes := map[string]bool{}
	var dicts []*pyDict
	for _, item := range components {
		component, ok := dictItems(item)
		if !ok {
			return nil, fmt.Errorf("a compatibility component is not an object")
		}
		dicts = append(dicts, component)
		shapes[strings.Join(component.sortedKeys(), "\x00")] = true
	}
	if len(shapes) > 1 {
		problems = append(problems, "compatibility components do not share one field set")
	}
	for _, component := range dicts {
		name := "<unnamed>"
		if component.has("component") {
			value, ok := component.get("component").(string)
			if !ok {
				return nil, fmt.Errorf("a component name is not a string")
			}
			name = value
		}
		for _, key := range []string{"source", "revision", "tree", "version", "requiresPython", "installs"} {
			if !pyTruthy(component.get(key)) {
				problems = append(problems, name+" is missing a non-empty "+key)
			}
		}
		if !component.has("measuredPoints") {
			problems = append(problems, name+" does not state measuredPoints, not even as an empty list")
		} else if _, ok := component.get("measuredPoints").([]any); !ok {
			problems = append(problems, name+" states measuredPoints as something other than a list")
		}
		if !component.has("workingTreeClean") {
			problems = append(problems, name+" does not state workingTreeClean, which OPS-2.1 needs as a signal")
		}
		source := &pyDict{vals: map[string]any{}}
		if component.has("source") {
			d, ok := dictItems(component.get("source"))
			if !ok {
				return nil, fmt.Errorf("%s source is not an object", name)
			}
			source = d
		}
		if !pyTruthy(source.get("checkout")) {
			problems = append(problems, name+" has no source.checkout path")
		}
		for _, key := range []string{"revision", "tree"} {
			text := ""
			if component.has(key) {
				text = pyStr(component.get(key))
			}
			if len([]rune(text)) != 40 {
				kind := map[string]string{"revision": "commit", "tree": "tree"}[key]
				problems = append(problems, name+" "+key+" is not a full 40 character "+kind+" id")
			}
		}
		if !source.has("remote") {
			problems = append(problems, name+" does not state a remote, not even as none")
		}
		var installs []any
		if component.has("installs") {
			list, ok := component.get("installs").([]any)
			if !ok {
				return nil, fmt.Errorf("%s installs is not a list", name)
			}
			installs = list
		}
		for _, item := range installs {
			install, ok := dictItems(item)
			if !ok {
				return nil, fmt.Errorf("%s has an install that is not an object", name)
			}
			if mode, _ := install.get("installMode").(string); mode != "editable" && mode != "copied" {
				problems = append(problems, name+" has an install with an undeclared installMode")
			}
			integrity := ""
			if install.has("integrity") {
				integrity = pyStr(install.get("integrity"))
			}
			if len([]rune(integrity)) != 64 {
				problems = append(problems, name+" has an install without a 64 character integrity digest")
			}
			for _, key := range []string{"environment", "location", "entryPoint"} {
				if !pyTruthy(install.get(key)) {
					problems = append(problems, name+" has an install with no "+key+", so OPS-1.1 cannot separate the environment from the imported package")
				}
			}
		}
		points, _ := component.get("measuredPoints").([]any)
		for _, item := range points {
			point, ok := dictItems(item)
			if !ok {
				problems = append(problems, name+" has a measured point that is not an object")
				continue
			}
			for _, key := range opsPointKeys {
				if !point.has(key) {
					problems = append(problems, name+" has a measured point missing "+key)
				}
			}
		}
	}
	if !record.has("unmeasured") {
		problems = append(problems, "the compatibility example does not say what is unmeasured, which invites a range claim")
	}
	return problems, nil
}

func opsScenarios(text string, problems []string) (int, []string) {
	blocks := map[string][]string{}
	current := ""
	started := false
	for _, line := range pySplitlines(text) {
		if m := opsScenario.FindStringSubmatch(line); m != nil {
			current, started = m[1], true
			blocks[current] = []string{}
		} else if started {
			blocks[current] = append(blocks[current], line)
		}
	}
	if len(blocks) < 6 {
		problems = append(problems, "only "+strconv.Itoa(len(blocks))+" scenarios found, the contract requires at least the six named situations")
	}
	for _, name := range sortedKeys(blocks) {
		body := []rune(strings.Join(blocks[name], "\n"))
		type position struct {
			part  string
			index int
		}
		var ordered []position
		for _, part := range opsParts {
			index := strings.Index(string(body), part)
			if index < 0 {
				problems = append(problems, "scenario "+name+" is missing its "+strings.TrimSuffix(part, ":")+" part")
				continue
			}
			ordered = append(ordered, position{part, len([]rune(string(body)[:index]))})
		}
		sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].index < ordered[j].index })
		for offset, p := range ordered {
			end := len(body)
			if offset+1 < len(ordered) {
				end = ordered[offset+1].index
			}
			start := min(p.index+len([]rune(p.part)), len(body))
			written := 0
			if start < end {
				written = len([]rune(strings.Join(strings.FieldsFunc(string(body[start:end]), pyIsSpace), "")))
			}
			if minimum := opsMinimum[p.part]; written < minimum {
				problems = append(problems, "scenario "+name+" states its "+strings.TrimSuffix(p.part, ":")+" part in fewer than "+strconv.Itoa(minimum)+" characters")
			}
		}
		if !opsClause.MatchString(string(body)) {
			problems = append(problems, "scenario "+name+" cites no clause")
		}
	}
	return len(blocks), problems
}

// OperationsContract is `crw-dev ci operations` (scripts/check_operations_contract.py).
func OperationsContract(args []string, stdout, stderr io.Writer) int {
	values, present, code := parseOptions("operations", "Replay the operations fixtures against the contract they claim to follow.",
		[]string{"root"}, nil, args, stdout, stderr)
	if code >= 0 {
		return code
	}
	root := values["root"]
	if !present["root"] {
		top, err := repositoryRoot()
		if err != nil {
			return failf(stderr, "operations: %s", err)
		}
		root = top
	}
	return operationsCheck(root, stdout, stderr)
}

func operationsCheck(root string, stdout, stderr io.Writer) int {
	references := filepath.Join(root, "plugins", "crw", "skills", "crw-run", "references")
	contractPath := filepath.Join(references, "operations.md")
	fixtures := filepath.Join(references, "operations")
	missing := false
	for _, path := range []string{contractPath, fixtures} {
		if _, err := os.Stat(path); err != nil {
			fmt.Fprintln(stderr, "MISSING "+path)
			missing = true
		}
	}
	if missing {
		return 1
	}
	crash := func(err error) int {
		fmt.Fprintf(stderr, "operations: %s\n", err)
		return 1
	}
	var problems []string
	contract, err := readText(contractPath)
	if err != nil {
		return crash(err)
	}
	defined, normative := contractClauses(contract)
	scenarioText, err := readText(filepath.Join(fixtures, "scenarios.md"))
	if err != nil {
		return crash(err)
	}
	if _, err := readText(filepath.Join(fixtures, "installation-plan.example.md")); err != nil {
		return crash(err)
	}
	blockCount, problems := opsScenarios(scenarioText, problems)
	entries, err := os.ReadDir(fixtures)
	if err != nil {
		return crash(valueErrorOS(err))
	}
	cited := map[string]bool{}
	for _, e := range entries {
		path := filepath.Join(fixtures, e.Name())
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			continue
		}
		text, err := readText(path)
		if err != nil {
			return crash(err)
		}
		for _, clause := range opsClause.FindAllString(text, -1) {
			cited[clause] = true
		}
	}
	for _, clause := range sortedKeys(cited) {
		if !defined[clause] {
			problems = append(problems, "fixtures cite "+clause+", which the contract does not define")
		}
	}
	for _, clause := range sortedKeys(normative) {
		if !cited[clause] {
			problems = append(problems, clause+" is never exercised by a fixture")
		}
	}
	records := make([]*pyDict, 2)
	var decodeErr error
	for i, name := range []string{"compatibility-record.example.json", "check-result.example.json"} {
		text, err := readText(filepath.Join(fixtures, name))
		if err != nil {
			return crash(err)
		}
		value, err := pyJSONLoadsOrdered(text)
		if err != nil {
			decodeErr = err
			break
		}
		record, ok := asDict(value)
		if !ok {
			return crash(fmt.Errorf("%s is not a JSON object", name))
		}
		records[i] = record
	}
	if decodeErr != nil {
		problems = append(problems, "an example record is not valid JSON: "+decodeErr.Error())
	} else {
		if problems, err = compatibilityRecord(records[0], problems); err != nil {
			return crash(err)
		}
		if problems, err = checkResultRecord(contract, records[1], problems); err != nil {
			return crash(err)
		}
	}
	fmt.Fprintf(stdout, "contract: %d clause ids, %d normative\n", len(defined), len(normative))
	fmt.Fprintf(stdout, "fixtures: %d scenarios, %d distinct clause citations\n", blockCount, len(cited))
	if len(problems) > 0 {
		fmt.Fprintln(stderr)
		for _, problem := range problems {
			fmt.Fprintln(stderr, "FAIL "+problem)
		}
		fmt.Fprintf(stderr, "\n%d problem(s). Nothing was modified.\n", len(problems))
		return 1
	}
	fmt.Fprintln(stdout, "OK every clause cited by any fixture exists, every normative clause is cited, every "+
		"scenario states all four parts, and both example records match the fields the contract "+
		"declares. This is citation and shape only: it does not read any fixture for meaning.")
	return 0
}
