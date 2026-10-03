// Package job is a red-first stub: signatures only, zero behaviour. The next commit replaces it.
package job

import (
	"encoding/json"
	"time"
)

const (
	BGDirName     = "bg"
	DisabledFile  = "disabled"
	EnabledAtFile = "enabled-at"
	LedgerFile    = "ledger.jsonl"
)

type sentinel string

func (e sentinel) Error() string { return string(e) }

const ErrOutsideStore = sentinel("stub")

func BGDir(cwd string) string                                     { return "" }
func RecordPath(cwd, id string) string                            { return "" }
func OutPath(cwd, id string) string                               { return "" }
func ExitPath(cwd, id string) string                              { return "" }
func DisabledPath(cwd string) string                              { return "" }
func EnabledAtPath(cwd string) string                             { return "" }
func EnsureDir(cwd string) (string, error)                        { return "", nil }
func AtomicWrite(cwd, path, text string) error                    { return nil }
func atomicWrite(cwd, path, text string, pid int, ms int64) error { return nil }
func ReadText(path string) (string, bool)                         { return "", false }
func ReadJSON(path string) (json.RawMessage, bool)                { return nil, false }

type Member struct {
	Key   string
	Value any
}
type Event []Member

func AppendLedger(cwd string, event Event)                             {}
func appendLedger(cwd string, event Event, now func() time.Time) error { return nil }
func ListRecordIDs(cwd string) []string                                { return nil }
func MtimeMs(path string) (float64, bool)                              { return 0, false }
func msOf(t time.Time) float64                                         { return 0 }
func RemovePath(cwd, path string) error                                { return nil }
