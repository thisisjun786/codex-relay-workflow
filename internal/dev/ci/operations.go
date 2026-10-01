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

// The operations fixtures replayed against the contract they claim to follow: citation and
// shape only, never the fixtures' meaning.
var (
	opsClause      = regexp.MustCompile(`OPS-\d+(?:\.\d+)?`)
	opsHeading     = regexp.MustCompile(`^#{2,3}\s+(OPS-\d+(?:\.\d+)?)\b`)
	opsNormative   = regexp.MustCompile(`^###\s+(OPS-\d+\.\d+)\b`)
	opsRegisterRow = regexp.MustCompile(`^\|\s*(OPS-\d+\.\d+)\s*\|`)
	opsTicked      = regexp.MustCompile("`([A-Za-z][A-Za-z0-9_]*)`")
	opsScenario    = regexp.MustCompile(`^##\s+(S\d+)\s+(.+)$`)
	opsParts       = []string{"Observed:", "Clauses:", "Action:", "Preserved:"}
	opsMinimum     = map[string]int{"Observed:": 40, "Clauses:": 7, "Action:": 80, "Preserved:": 40}
	opsFieldValues = []string{"verified", "not_verified", "unknown", "not_applicable"}
	// OPS-1.3: a point names the install it covers and the bytes that ran; a Python-era point
	// (the fence installer's, until the Python execution path is removed) names its interpreter
	// instead, because there the interpreter is part of the combination.
	opsGoPointKeys     = []string{"install", "installDigest", "codexCli", "appServer", "host", "date", "measuredBy", "method"}
	opsPythonPointKeys = []string{"interpreter", "codexCli", "appServer", "host", "date", "measuredBy", "method"}
)

func contractClauses(text string) (map[string]bool, map[string]bool) {
	defined, normative := map[string]bool{}, map[string]bool{}
	for _, line := range lines(text) {
		if m := opsHeading.FindStringSubmatch(line); m != nil {
			defined[m[1]] = true
		}
		if m := opsNormative.FindStringSubmatch(line); m != nil {
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

func declaredCheckFields(contract string) map[string]bool {
	names := map[string]bool{}
	inside := false
	for _, line := range lines(contract) {
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

// A record that is not the shape the check reads (a member that is not an object or a list)
// is reported as malformed and the check exits 1.

func checkResultRecord(contract string, record map[string]any, problems []string) ([]string, error) {
	declared := declaredCheckFields(contract)
	fields := map[string]any{}
	if has(record, "fields") {
		d, ok := object(record["fields"])
		if !ok {
			return nil, fmt.Errorf("check-result fields is not an object")
		}
		fields = d
	}
	present := map[string]bool{}
	for key := range fields {
		present[key] = true
	}
	if len(declared) == 0 {
		problems = append(problems, "OPS-6.1 declares no fields, so the check-result example cannot be verified")
	} else if !sameSet(declared, present) {
		problems = append(problems, "check-result fields "+show(sortedKeys(present))+" do not match OPS-6.1 "+show(sortedKeys(declared)))
	}
	for _, name := range sortedKeys(fields) {
		field, ok := object(fields[name])
		if !ok {
			return nil, fmt.Errorf("check-result field %s is not an object", name)
		}
		for _, key := range []string{"value", "evidence", "command", "actor", "measuredAt"} {
			if !has(field, key) {
				problems = append(problems, "check-result field "+name+" is missing "+key)
			}
		}
		value, isString := field["value"].(string)
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

func compatibilityRecord(record map[string]any, problems []string) ([]string, error) {
	var components []any
	if has(record, "components") {
		list, ok := record["components"].([]any)
		if !ok {
			return nil, fmt.Errorf("compatibility components is not a list")
		}
		components = list
	}
	if len(components) == 0 {
		problems = append(problems, "the compatibility example records no component")
	}
	shapes := map[string]bool{}
	var dicts []map[string]any
	for _, item := range components {
		component, ok := object(item)
		if !ok {
			return nil, fmt.Errorf("a compatibility component is not an object")
		}
		dicts = append(dicts, component)
		shapes[strings.Join(sortedKeys(component), "\x00")] = true
	}
	if len(shapes) > 1 {
		problems = append(problems, "compatibility components do not share one field set")
	}
	for _, component := range dicts {
		name := "<unnamed>"
		if has(component, "component") {
			value, ok := component["component"].(string)
			if !ok {
				return nil, fmt.Errorf("a component name is not a string")
			}
			name = value
		}
		for _, key := range []string{"source", "revision", "tree", "version", "installs"} {
			if !filled(component[key]) {
				problems = append(problems, name+" is missing a non-empty "+key)
			}
		}
		// OPS-1.1: requires-python belongs to a Python-era install, which records an install
		// mode; a Go install is a release binary and declares no interpreter at all.
		pythonEra := false
		if list, ok := component["installs"].([]any); ok {
			for _, item := range list {
				if install, ok := object(item); ok && has(install, "installMode") {
					pythonEra = true
				}
			}
		}
		if pythonEra && !filled(component["requiresPython"]) {
			problems = append(problems, name+" is missing a non-empty requiresPython, which a Python-era install needs")
		}
		if !has(component, "measuredPoints") {
			problems = append(problems, name+" does not state measuredPoints, not even as an empty list")
		} else if _, ok := component["measuredPoints"].([]any); !ok {
			problems = append(problems, name+" states measuredPoints as something other than a list")
		}
		if !has(component, "workingTreeClean") {
			problems = append(problems, name+" does not state workingTreeClean, which OPS-2.1 needs as a signal")
		}
		source := map[string]any{}
		if has(component, "source") {
			d, ok := object(component["source"])
			if !ok {
				return nil, fmt.Errorf("%s source is not an object", name)
			}
			source = d
		}
		if !filled(source["checkout"]) {
			problems = append(problems, name+" has no source.checkout path")
		}
		for _, key := range []string{"revision", "tree"} {
			id := ""
			if has(component, key) {
				id = text(component[key])
			}
			if len([]rune(id)) != 40 {
				kind := map[string]string{"revision": "commit", "tree": "tree"}[key]
				problems = append(problems, name+" "+key+" is not a full 40 character "+kind+" id")
			}
		}
		if !has(source, "remote") {
			problems = append(problems, name+" does not state a remote, not even as none")
		}
		var installs []any
		if has(component, "installs") {
			list, ok := component["installs"].([]any)
			if !ok {
				return nil, fmt.Errorf("%s installs is not a list", name)
			}
			installs = list
		}
		var goInstalls []map[string]any
		for _, item := range installs {
			install, ok := object(item)
			if !ok {
				return nil, fmt.Errorf("%s has an install that is not an object", name)
			}
			if has(install, "installMode") {
				if mode, _ := install["installMode"].(string); mode != "editable" && mode != "copied" {
					problems = append(problems, name+" has an install with an undeclared installMode")
				}
			} else {
				goInstalls = append(goInstalls, install)
				// A Go install: the binary's own digest and the target it was built for.
				digest := ""
				if has(install, "binaryDigest") {
					digest = text(install["binaryDigest"])
				}
				if len([]rune(digest)) != 64 {
					problems = append(problems, name+" has a Go install without a 64 character binaryDigest")
				}
				if !filled(install["target"]) {
					problems = append(problems, name+" has a Go install that does not name its target")
				}
			}
			integrity := ""
			if has(install, "integrity") {
				integrity = text(install["integrity"])
			}
			if len([]rune(integrity)) != 64 {
				problems = append(problems, name+" has an install without a 64 character integrity digest")
			}
			for _, key := range []string{"environment", "location", "entryPoint"} {
				if !filled(install[key]) {
					problems = append(problems, name+" has an install with no "+key+", so OPS-1.1 cannot separate the environment from the imported package")
				}
			}
		}
		points, _ := component["measuredPoints"].([]any)
		for _, item := range points {
			point, ok := object(item)
			if !ok {
				problems = append(problems, name+" has a measured point that is not an object")
				continue
			}
			pythonPoint := has(point, "interpreter")
			keys := opsGoPointKeys
			if pythonPoint {
				keys = opsPythonPointKeys
			}
			for _, key := range keys {
				if !has(point, key) {
					problems = append(problems, name+" has a measured point missing "+key)
				}
			}
			// OPS-1.3: a point takes the kind of the install it covers, so its shape alone proves
			// nothing. A Python-era point needs a Python-era install beside it; a Go point names one
			// of this component's Go installs by location and the digest of that binary's bytes.
			if pythonPoint {
				if !pythonEra {
					problems = append(problems, name+" has a Python-era measured point but no Python-era install for it to cover")
				}
				continue
			}
			var covered map[string]any
			if want, ok := point["install"].(string); ok {
				for _, install := range goInstalls {
					if location, ok := install["location"].(string); ok && location == want {
						covered = install
						break
					}
				}
			}
			if covered == nil {
				problems = append(problems, name+" has a measured point that names no Go install of this component")
				continue
			}
			digest, digestOK := point["installDigest"].(string)
			if binary, ok := covered["binaryDigest"].(string); !digestOK || !ok || binary != digest {
				problems = append(problems, name+" has a measured point whose installDigest is not its install's binaryDigest")
			}
		}
	}
	if !has(record, "unmeasured") {
		problems = append(problems, "the compatibility example does not say what is unmeasured, which invites a range claim")
	}
	return problems, nil
}

func opsScenarios(text string, problems []string) (int, []string) {
	blocks := map[string][]string{}
	current := ""
	started := false
	for _, line := range lines(text) {
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
				written = len([]rune(strings.Join(strings.Fields(string(body[start:end])), "")))
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

// OperationsContract is `crw-dev ci operations`.
func OperationsContract(args []string, stdout, stderr io.Writer) int {
	flags := newFlags("operations")
	root := flags.String("root", "", "the checkout to check (default: the one holding the working directory)")
	if code := parseFlags(flags, "Replay the operations fixtures against the contract they claim to follow.", args, stdout, stderr); code >= 0 {
		return code
	}
	if !given(flags, "root") {
		top, err := repositoryRoot()
		if err != nil {
			return failf(stderr, "operations: %s", err)
		}
		*root = top
	}
	return operationsCheck(*root, stdout, stderr)
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
		return crash(err)
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
	records := make([]map[string]any, 2)
	var decodeErr error
	for i, name := range []string{"compatibility-record.example.json", "check-result.example.json"} {
		text, err := readText(filepath.Join(fixtures, name))
		if err != nil {
			return crash(err)
		}
		value, err := decodeJSON([]byte(text))
		if err != nil {
			decodeErr = err
			break
		}
		record, ok := object(value)
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
