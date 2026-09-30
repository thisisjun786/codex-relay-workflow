// Package linkage holds the Go port of test_linkage.py and its sibling suites: every contract and
// behaviour property of the three-level linkage, asserted against each scenario's golden
// (testdata/golden, which began as the answers the Python relay gave for the same scenario and
// is regenerated with CRW_GOLDEN=update). The implementation lives in internal/relay/registry,
// beside the binding plan registration uses.
package linkage
