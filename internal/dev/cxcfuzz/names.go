//go:build dev

package cxcfuzz

import (
	"sort"
	"sync"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/cxccorpus"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

var (
	substitutionOnce  sync.Once
	substitutionTable *cxccorpus.Substituter
	substitutionErr   error
)

// sharedSubstitution is the corpus rename table, read once per process the first time a comparison
// needs it. Reading it at package start would do work in every dev binary, and reading it per
// comparison would parse the table once per case.
func sharedSubstitution() (*cxccorpus.Substituter, error) {
	substitutionOnce.Do(func() { substitutionTable, substitutionErr = doctorSubstitution() })
	return substitutionTable, substitutionErr
}

// normaliseDiagnosticPaths applies the rename table to the path of a read diagnostic. The oracle reads
// its state under .codexclaw and the port under .crw, and the table's directory rule (R26) maps the one
// onto the other, so both sides name the same directory before anything is compared or reduced
// (CRW-978 c1). Only the path is renamed; the kind, field and detail compare as they are.
func normaliseDiagnosticPaths(sub *cxccorpus.Substituter, out any) any {
	object, ok := out.(pyjson.Object)
	if !ok {
		return out
	}
	renamed := make(pyjson.Object, 0, len(object))
	for _, item := range object {
		if text, ok := item.Value.(string); ok && item.Key == "path" {
			renamed = append(renamed, pyjson.Field{Key: item.Key, Value: sub.Apply(text)})
			continue
		}
		renamed = append(renamed, item)
	}
	return renamed
}

// differingFields names the top-level fields whose canonical values differ between two answers, sorted.
// It is what a verdict says differs, so a difference that normalises away is never listed.
func differingFields(goOut, oracleOut any) []string {
	goObject, goOK := goOut.(pyjson.Object)
	oracleObject, oracleOK := oracleOut.(pyjson.Object)
	if !goOK || !oracleOK {
		return nil
	}
	names := map[string]bool{}
	for _, item := range goObject {
		names[item.Key] = true
	}
	for _, item := range oracleObject {
		names[item.Key] = true
	}
	differing := []string{}
	for name := range names {
		goValue, goFound := goObject.Lookup(name)
		oracleValue, oracleFound := oracleObject.Lookup(name)
		if goFound != oracleFound || (goFound && canonical(goValue) != canonical(oracleValue)) {
			differing = append(differing, name)
		}
	}
	sort.Strings(differing)
	return differing
}
