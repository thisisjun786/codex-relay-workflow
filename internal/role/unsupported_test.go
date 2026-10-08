package role

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// nl is the newline a stub script is assembled with, kept out of the literals so the script's own
// lines stay readable.
const nl = "\n"

// TestLiveCatalogTellsUnsupportedFromAFailure is C4: an OCX that does not answer the live-catalog
// command is a different state from a catalog that could not be read. Reporting both as unavailable
// hides a host whose OCX simply has no such command, and reporting the first as stale reuses a cache
// for a question the tool cannot answer at all.
func TestLiveCatalogTellsUnsupportedFromAFailure(t *testing.T) {
	t.Run("unsupported without a cache", func(t *testing.T) {
		o := liveOptions(t)
		o.RunOcx = func([]string) (string, error) { return "", ErrCatalogUnsupported }
		var r CatalogReader
		c := liveRead(t, &r, o)
		if c.State != CatalogUnsupported {
			t.Fatalf("state = %q, want %q", c.State, CatalogUnsupported)
		}
		if c.Status != "unavailable" {
			t.Fatalf("status = %q, want unavailable", c.Status)
		}
		if len(c.Entries) != 0 {
			t.Fatalf("entries = %v, want none", c.Entries)
		}
	})

	t.Run("unsupported with a cache", func(t *testing.T) {
		o := liveOptions(t)
		o.RunOcx = func([]string) (string, error) { return liveRoster("cached"), nil }
		var r CatalogReader
		if c := liveRead(t, &r, o); c.Status != "fresh" {
			t.Fatalf("seed read = %+v", c)
		}
		o.ForceRefresh = true
		o.RunOcx = func([]string) (string, error) { return "", ErrCatalogUnsupported }
		c := liveRead(t, &r, o)
		if c.State != CatalogUnsupported {
			t.Fatalf("state = %q, want %q", c.State, CatalogUnsupported)
		}
		if c.Status != "stale" || len(c.Entries) == 0 || c.Entries[0].ID != "cached" {
			t.Fatalf("the cached list was not carried: %+v", c)
		}
		// The marshalled answer must carry the unsupported state, not the cached state: the cached
		// object is merged into the response, so overriding the struct field alone would be lost.
		wire := string(must(Stringify(c, "")))
		if !strings.Contains(wire, "\""+string(CatalogUnsupported)+"\"") {
			t.Fatalf("the wire answer does not carry the unsupported state: %s", wire)
		}
	})

	t.Run("a read failure is not unsupported", func(t *testing.T) {
		o := liveOptions(t)
		o.RunOcx = func([]string) (string, error) { return "", errors.New("discovery failed") }
		var r CatalogReader
		if c := liveRead(t, &r, o); c.State != CatalogUnavailable || c.Status != "unavailable" {
			t.Fatalf("a read failure answered %+v, want state %q", c, CatalogUnavailable)
		}
	})

	t.Run("a read failure with a cache stays stale, not unsupported", func(t *testing.T) {
		o := liveOptions(t)
		o.RunOcx = func([]string) (string, error) { return liveRoster("cached"), nil }
		var r CatalogReader
		liveRead(t, &r, o)
		o.ForceRefresh = true
		o.RunOcx = func([]string) (string, error) { return "", errors.New("discovery failed") }
		c := liveRead(t, &r, o)
		if c.State == CatalogUnsupported {
			t.Fatalf("a read failure was reported unsupported: %+v", c)
		}
		if c.Status != "stale" {
			t.Fatalf("status = %q, want stale", c.Status)
		}
	})
}

// TestRunOcxModelsClassifiesTheCommandRejection is C4's reader half: the OCX that refuses the
// command line itself is the unsupported case, and a runtime failure is not. The two are told apart
// by the argument rejection's own usage block, because both exit non-zero with empty output.
func TestRunOcxModelsClassifiesTheCommandRejection(t *testing.T) {
	o := liveOptions(t)
	bin, _ := catalogEnv(o.Environ)("PATH")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	writeStub := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(bin, "ocx"), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// The real argument rejection: non-zero exit, nothing on stdout, a usage block on stderr.
	writeStub("#!/bin/sh" + nl + "echo 'Unexpected argument(s): live' >&2" + nl +
		"echo 'Usage: ocx models [--provider <name>] [--json]' >&2" + nl + "exit 1" + nl)
	if _, err := RunOcxModels(o.Environ); !errors.Is(err, ErrCatalogUnsupported) {
		t.Fatalf("an argument rejection was not classified unsupported: %v", err)
	}
	// A runtime failure: non-zero exit, a message, and no usage block.
	writeStub("#!/bin/sh" + nl + "echo 'Error: Proxy is not running. Start the intended proxy with: ocx start.' >&2" + nl + "exit 1" + nl)
	if _, err := RunOcxModels(o.Environ); errors.Is(err, ErrCatalogUnsupported) || err == nil {
		t.Fatalf("a runtime failure was classified unsupported: %v", err)
	}
	// A capable OCX whose command fails while running, with partial output, is not the rejection.
	writeStub("#!/bin/sh" + nl + "echo 'partial'" + nl + "echo 'Usage: ocx models [--provider <name>] [--json]' >&2" + nl + "exit 1" + nl)
	if _, err := RunOcxModels(o.Environ); errors.Is(err, ErrCatalogUnsupported) {
		t.Fatal("a failure that produced output was classified unsupported")
	}
}
