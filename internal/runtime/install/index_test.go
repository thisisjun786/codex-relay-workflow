package install_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/swapgate"
)

// CRW-472: ordinary indexes on tables the store already holds arrive through the OPS-4.5 backup route the zone uses, and
// leave without it; everything else refuses as it always did, with the backup acknowledged and no backup made.

// idxCandidate is a candidate that declares what this build declares, changed by mutate (nil for no change).
func idxCandidate(mutate func(record.Object) record.Object) func(context.Context, string) record.Object {
	return func(context.Context, string) record.Object {
		objects := golden.Obj(record.Get(swapgate.DeclaredSchema(context.Background()), "objects"))
		if mutate != nil {
			objects = mutate(objects)
		}
		return record.Object{{Key: "readable", Value: true}, {Key: "objects", Value: objects}, {Key: "schemaVersion", Value: store.SchemaVersion}, {Key: "detail", Value: nil}}
	}
}

// idxAdding adds objects to what the candidate declares, as key and statement pairs.
func idxAdding(pairs ...string) func(record.Object) record.Object {
	return func(objects record.Object) record.Object {
		for i := 0; i < len(pairs); i += 2 {
			objects = append(objects, record.Object{{Key: pairs[i], Value: pairs[i+1]}}...)
		}
		return objects
	}
}

const (
	idxOrdinary    = "index crw472_attempts_observed"
	idxOrdinarySQL = "CREATE INDEX crw472_attempts_observed ON attempts (observed_at)"
)

// idxHost is a host whose installed runtime is this build, with a store this build opened (the zone is in it) beside the
// files a state directory holds, so a backup has something to copy.
func idxHost(t *testing.T) (h *host, second, old, next string) {
	t.Helper()
	h, _, second, old, next = zoneInstalled(t)
	zoneStore(t, h)
	openedByThisBuild(t, h)
	return h, second, old, next
}

// idxExec runs statements against the host's store.
func idxExec(t *testing.T, h *host, statements ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(h.relayState, "relay.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

func TestAnIndexArrivalIsRefusedWithoutTheRouteAndPassesWithIt(t *testing.T) {
	h, second, old, next := idxHost(t)
	before := digestTree(t, h.relayState)
	o := h.options()
	o.CandidateSchema = idxCandidate(idxAdding(idxOrdinary, idxOrdinarySQL))

	refused, code := install.Install(context.Background(), o, "update", install.Source{From: second})
	cell := golden.Canon(at(refused, "swapGate", "cells", "storeSchema"))
	if code != install.Refused || at(refused, "failedStep") != "read whether it is safe to swap" || at(refused, "swapGate", "verdict") != "BLOCKED" ||
		at(refused, "swapGate", "cells", "storeSchema", "answer") != swapgate.ExtendsIndex || !strings.Contains(cell, "--backup-state-to") || !strings.Contains(cell, idxOrdinary) {
		t.Fatalf("without the route: exit %d\n%s", code, golden.Canon(at(refused, "swapGate")))
	}
	if h.pointerTarget(t) != old {
		t.Fatalf("the pointer moved to %s", h.pointerTarget(t))
	}
	sameTree(t, withoutSidecars(before), withoutSidecars(digestTree(t, h.relayState)), "the state directory")

	seen, restore := atCopy(t, h)
	defer restore()
	o.StateBackup = backupOf(h, "index-arrival")
	result, code := install.Install(context.Background(), o, "update", install.Source{From: second})
	if code != install.OK || at(result, "promoted") != true || at(result, "swapGate", "verdict") != "ALLOWED" || at(result, "swapGate", "stateBackup", "made") != true ||
		at(result, "swapGate", "cells", "storeSchema", "answer") != swapgate.ExtendsIndex {
		t.Fatalf("with the route: exit %d\n%s", code, golden.Canon(at(result, "swapGate")))
	}
	if h.pointerTarget(t) != next {
		t.Fatalf("the swap did not happen: %s", h.pointerTarget(t))
	}
	sameTree(t, *seen, digestTree(t, backupOf(h, "index-arrival")), "the backup")
	if !strings.Contains(string(mustRead(t, backupOf(h, "index-arrival")+install.ManifestSuffix)), "ordinary indexes") {
		t.Fatal("the manifest does not say what the copy was taken for")
	}
}

// The route carries ordinary indexes and nothing that merely resembles them: whatever else arrives, alone or beside an
// ordinary index, refuses with the acknowledgement as without it, and no backup is made for a swap that does not happen.
func TestTheRouteDoesNotCarryAUniqueIndexANewTableAColumnChangeATriggerOrAFunction(t *testing.T) {
	changeColumns := func(objects record.Object) record.Object {
		out := make(record.Object, len(objects))
		copy(out, objects)
		for i, field := range out {
			if statement, ok := field.Value.(string); ok && field.Key == "table deliveries" {
				changed := strings.Replace(statement, "CREATE TABLE deliveries (", "CREATE TABLE deliveries (crw472_extra TEXT, ", 1)
				if changed == statement {
					t.Fatalf("the statement of deliveries is not what this test changes: %s", statement)
				}
				out[i].Value = changed
			}
		}
		return append(out, record.Object{{Key: "index crw472_extra_index", Value: "CREATE INDEX crw472_extra_index ON deliveries (crw472_extra)"}}...)
	}
	for name, tc := range map[string]struct {
		mutate func(record.Object) record.Object
		answer string
	}{
		"a unique index":                        {idxAdding("index crw472_u", "CREATE UNIQUE INDEX crw472_u ON attempts (request_id, observed_at)"), swapgate.Extends},
		"an ordinary index beside a unique one": {idxAdding(idxOrdinary, idxOrdinarySQL, "index crw472_u", "CREATE UNIQUE INDEX crw472_u ON attempts (request_id, observed_at)"), swapgate.Extends},
		"a new table":                           {idxAdding("table crw472_new", "CREATE TABLE crw472_new (x)"), swapgate.Extends},
		"an index beside a new table":           {idxAdding(idxOrdinary, idxOrdinarySQL, "table crw472_new", "CREATE TABLE crw472_new (x)"), swapgate.Extends},
		"a trigger":                             {idxAdding("trigger crw472_t", "CREATE TRIGGER crw472_t AFTER INSERT ON attempts BEGIN SELECT 1; END"), swapgate.Extends},
		"an index that calls a function":        {idxAdding("index crw472_f", "CREATE INDEX crw472_f ON refusals (json_extract(reason, '$.code'))"), swapgate.Extends},
		"a column change with an index on it":   {changeColumns, swapgate.Differs},
	} {
		t.Run(name, func(t *testing.T) {
			h, second, old, _ := idxHost(t)
			o := h.options()
			o.CandidateSchema = idxCandidate(tc.mutate)
			o.StateBackup = backupOf(h, "refused")
			result, code := install.Install(context.Background(), o, "update", install.Source{From: second})
			if code != install.Refused || at(result, "swapGate", "verdict") != "BLOCKED" || at(result, "swapGate", "cells", "storeSchema", "answer") != tc.answer {
				t.Fatalf("exit %d\n%s", code, golden.Canon(at(result, "swapGate")))
			}
			if h.pointerTarget(t) != old {
				t.Fatal("the pointer moved")
			}
			nothingAt(t, backupOf(h, "refused"))
			nothingAt(t, backupOf(h, "refused")+install.ManifestSuffix)
			if at(result, "swapGate", "stateBackup", "made") != false {
				t.Fatalf("a backup was reported: %s", golden.Canon(at(result, "swapGate", "stateBackup")))
			}
		})
	}
}

// An ordinary index the candidate does not declare leaves with an update and with a rollback, with no acknowledgement and no
// backup; an index that is unique, or calls a function, still refuses.
func TestAnIndexDepartureIsNotRefused(t *testing.T) {
	t.Run("an update", func(t *testing.T) {
		h, second, _, next := idxHost(t)
		idxExec(t, h, "CREATE INDEX crw472_extra ON attempts (observed_at)")
		result, code := install.Install(context.Background(), h.options(), "update", install.Source{From: second})
		if code != install.OK || at(result, "swapGate", "cells", "storeSchema", "answer") != swapgate.NarrowsIndex || at(result, "swapGate", "verdict") != "ALLOWED" || h.pointerTarget(t) != next {
			t.Fatalf("exit %d\n%s", code, golden.Canon(at(result, "swapGate")))
		}
		if at(result, "swapGate", "stateBackup") != nil {
			t.Fatalf("a backup was reported: %s", golden.Canon(at(result, "swapGate", "stateBackup")))
		}
	})
	t.Run("a rollback", func(t *testing.T) {
		h, _, second, old, _ := zoneInstalled(t)
		h.mustInstall(t, "update", second)
		openedByThisBuild(t, h)
		idxExec(t, h, "CREATE INDEX crw472_extra ON attempts (observed_at)")
		result, code := install.Rollback(context.Background(), h.options(), "")
		if code != install.OK || at(result, "swapGate", "cells", "storeSchema", "answer") != swapgate.NarrowsIndex || h.pointerTarget(t) != old {
			t.Fatalf("exit %d\n%s", code, golden.Canon(result))
		}
	})
	for name, statement := range map[string]string{
		"a unique index":                 "CREATE UNIQUE INDEX crw472_extra ON attempts (request_id, observed_at)",
		"an index that calls a function": "CREATE INDEX crw472_extra ON refusals (json_extract(reason, '$.code'))",
	} {
		t.Run(name+" still refuses", func(t *testing.T) {
			h, second, old, _ := idxHost(t)
			idxExec(t, h, statement)
			result, code := install.Install(context.Background(), h.options(), "update", install.Source{From: second})
			if code != install.Refused || at(result, "swapGate", "cells", "storeSchema", "answer") != swapgate.Narrows || h.pointerTarget(t) != old {
				t.Fatalf("exit %d\n%s", code, golden.Canon(at(result, "swapGate")))
			}
		})
	}
}

// idxArchive packs binary as a release archive of version, with the three names it is shipped under and a licence, and writes
// the SHA256SUMS beside it, as archive does for the binary these tests build.
func idxArchive(t *testing.T, binary []byte, version string) string {
	t.Helper()
	var buf bytes.Buffer
	compressed := gzip.NewWriter(&buf)
	w := tar.NewWriter(compressed)
	add := func(header *tar.Header, body []byte) {
		header.ModTime = time.Unix(0, 0)
		if err := w.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	add(&tar.Header{Name: "crw", Mode: 0o755, Size: int64(len(binary)), Typeflag: tar.TypeReg}, binary)
	for _, link := range []string{"codex-session-relay", "codex-thread-bridge"} {
		add(&tar.Header{Name: link, Linkname: "crw", Mode: 0o777, Typeflag: tar.TypeSymlink}, nil)
	}
	licence := []byte("MIT " + version + "\n")
	add(&tar.Header{Name: "LICENSE", Mode: 0o644, Size: int64(len(licence)), Typeflag: tar.TypeReg}, licence)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	name := "crw_" + version + "_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
	path := filepath.Join(t.TempDir(), name)
	write(t, path, buf.String())
	sum := sha256.Sum256(buf.Bytes())
	write(t, filepath.Join(filepath.Dir(path), install.SumsName), hex.EncodeToString(sum[:])+"  "+name+"\n")
	return path
}

// The same route with a real build as the candidate, on a temporary host: CRW_INDEX_CANDIDATE_BINARY names the crw of a build
// that declares an ordinary index the store this build opened lacks (the head of the pull request that adds the history
// indexes, at the time of CRW-472), and the test installs it as a release archive. Without the variable, or with a candidate
// that declares nothing the store lacks, there is nothing to show and the test is skipped. It runs nothing against the
// operating runtime, relay store or Codex home: the host is a temporary one and its relay is a fake.
//
//	CRW_INDEX_CANDIDATE_BINARY=/path/to/crw go test ./internal/runtime/install -run TestAReleaseBuildThatDeclaresIndexes -v
func TestAReleaseBuildThatDeclaresIndexesInstallsThroughTheClass(t *testing.T) {
	path := os.Getenv("CRW_INDEX_CANDIDATE_BINARY")
	if path == "" {
		t.Skip("CRW_INDEX_CANDIDATE_BINARY names no candidate build")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	h, _, _, old, _ := zoneInstalled(t)
	zoneStore(t, h)
	openedByThisBuild(t, h)
	candidate := idxArchive(t, raw, "index-candidate")
	next := runtimeDir(h, "index-candidate", candidate, t)

	refused, code := install.Install(context.Background(), h.options(), "update", install.Source{From: candidate})
	answer := at(refused, "swapGate", "cells", "storeSchema", "answer")
	if answer == swapgate.Agrees {
		t.Skip("the candidate declares nothing the store lacks")
	}
	t.Logf("without --backup-state-to: exit %d, verdict %v, storeSchema %v\n%s", code, at(refused, "swapGate", "verdict"), answer, golden.Canon(at(refused, "swapGate", "cells", "storeSchema")))
	if code != install.Refused || answer != swapgate.ExtendsIndex || h.pointerTarget(t) != old {
		t.Fatalf("exit %d\n%s", code, golden.Canon(at(refused, "swapGate")))
	}

	o := h.options()
	o.StateBackup = backupOf(h, "candidate")
	result, code := install.Install(context.Background(), o, "update", install.Source{From: candidate})
	t.Logf("with --backup-state-to: exit %d, verdict %v, storeSchema %v, backup made %v\n%s", code, at(result, "swapGate", "verdict"), at(result, "swapGate", "cells", "storeSchema", "answer"), at(result, "swapGate", "stateBackup", "made"), golden.Canon(at(result, "swapGate", "stateBackup")))
	if code != install.OK || at(result, "swapGate", "verdict") != "ALLOWED" || at(result, "swapGate", "stateBackup", "made") != true || h.pointerTarget(t) != next {
		t.Fatalf("exit %d\n%s", code, golden.Canon(at(result, "swapGate")))
	}
}
