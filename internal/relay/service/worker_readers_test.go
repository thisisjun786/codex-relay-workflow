package service_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/service"
)

// The reader managed-start admits through (adapter.WorkerObservation.Read) and the service's own
// (Service.ReadWorkerPolicy) are one reading behind two entries. worker_reasons_test.go pins both to
// the reason Python gave for every change a worker's files can show; this file holds them to one
// answer for files nothing writes (a record edited by hand, a receipt cut off, a number spelled
// another way) and for a change made while a reader is observing. Each answer below is written from
// what the file means, not taken from either reader: JSON is read strictly, only the worker-policy
// receipt has a size bound, an integer field takes an integer and every other number compares by
// value, and a token is any value that is not empty or false.
//
// Two shapes are left out on purpose: a repeated key (the service's record parser keeps repeats as
// fields of their own and answers from the first, where Python takes the last), which is recorded in
// docs/port/refactor-backlog.md rather than pinned here, and a socket symlink retargeted inside the
// observation, which needs a symlinked-socket fixture.

// rawText is a file's bytes as text.
func rawText(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// spell rewrites the nth (0-based) scalar value of key in a file, in text, as the literal given.
func spell(t *testing.T, f *workerFixture, path, key string, nth int, literal string) {
	t.Helper()
	text := rawText(t, path)
	locations := regexp.MustCompile(`"`+regexp.QuoteMeta(key)+`"\s*:\s*("[^"]*"|[^,}\]]+)`).FindAllStringIndex(text, -1)
	if nth >= len(locations) {
		t.Fatalf("%s has no occurrence %d of %q: %s", path, nth, key, text)
	}
	at := locations[nth]
	f.write(path, text[:at[0]]+`"`+key+`": `+literal+text[at[1]:])
}

func fieldOf(o contract.OrderedObject, key string) any {
	for _, field := range o {
		if field.Key == key {
			return field.Value
		}
	}
	return nil
}

// normalized is a policy as JSON values: the two readers hand back an ordered object and a map.
func normalized(t *testing.T, policy any) any {
	t.Helper()
	var raw []byte
	if o, ok := policy.(contract.OrderedObject); ok {
		raw = []byte(encode(t, o))
	} else {
		var err error
		if raw, err = json.Marshal(policy); err != nil {
			t.Fatal(err)
		}
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// answers asks both readers about f and returns each one's reason (nil is an observation) and
// the policy it observed.
func answers(t *testing.T, f *workerFixture) (service, managed *string, servicePolicy, managedPolicy any) {
	t.Helper()
	observation := f.service.ReadWorkerPolicy(context.Background())
	service, servicePolicy = observed(observation), fieldOf(observation, "policy")
	policy, reason := f.observer.Read(context.Background())
	managed = nullable(reason)
	if policy != nil {
		managedPolicy = policy
	}
	return service, managed, servicePolicy, managedPolicy
}

func TestWorkerPolicy_corrupted_files_have_one_answer_in_both_readers(t *testing.T) {
	policy := workerPolicy(t)
	pid := strconv.Itoa(os.Getpid())
	token := func(f *workerFixture, value any) {
		f.edit(f.recordPath, func(r map[string]any) { r["token"] = value })
		f.edit(f.scopeJSON, func(r map[string]any) { r["token"] = value })
		f.edit(f.receiptPath, func(r map[string]any) { r["service"].(map[string]any)["token"] = value })
	}
	const observedPolicy = ""
	for _, c := range []struct {
		name, reason string // reason is "" for an observation
		change       func(f *workerFixture)
	}{
		// Where managed-start's reader used to answer differently from the service's.
		{"receipt-service-pid-spelled-as-a-float", observedPolicy, func(f *workerFixture) { spell(t, f, f.receiptPath, "pid", 1, pid+".0") }},
		{"receipt-dbinode-spelled-as-a-float", observedPolicy, func(f *workerFixture) {
			text := rawText(t, f.receiptPath)
			m := regexp.MustCompile(`"dbInode"\s*:\s*([0-9]+)`).FindStringSubmatch(text)
			if m == nil {
				t.Fatal("the receipt names no dbInode")
			}
			f.write(f.receiptPath, strings.Replace(text, m[0], `"dbInode": `+m[1]+".0", 1))
		}},
		{"receipt-with-text-after-the-json", "worker_policy_unreadable", func(f *workerFixture) { f.write(f.receiptPath, rawText(t, f.receiptPath)+" x") }},
		{"record-with-text-after-the-json", "worker_policy_unreadable", func(f *workerFixture) { f.write(f.recordPath, rawText(t, f.recordPath)+" x") }},
		{"scope-claim-with-text-after-the-json", "worker_policy_scope_mismatch", func(f *workerFixture) { f.write(f.scopeJSON, rawText(t, f.scopeJSON)+" x") }},
		{"record-longer-than-the-receipt-bound", observedPolicy, func(f *workerFixture) { f.write(f.recordPath, rawText(t, f.recordPath)+strings.Repeat(" ", 70000)) }},
		{"token-is-a-number", observedPolicy, func(f *workerFixture) { token(f, 5) }},
		{"token-is-true", observedPolicy, func(f *workerFixture) { token(f, true) }},
		// Where they already agreed; kept so the one reading cannot drift on them.
		{"worker-pid-spelled-as-a-float", "worker_policy_process_mismatch", func(f *workerFixture) { spell(t, f, f.receiptPath, "pid", 0, pid+".0") }},
		{"record-is-an-empty-object", "worker_policy_process_mismatch", func(f *workerFixture) { f.write(f.recordPath, "{}") }},
		{"token-is-empty", "worker_policy_service_mismatch", func(f *workerFixture) { token(f, "") }},
		{"schema-version-spelled-as-a-float", "worker_policy_version_unknown", func(f *workerFixture) { spell(t, f, f.receiptPath, "schemaVersion", 0, "1.0") }},
		{"service-names-a-key-the-record-does-not", "worker_policy_service_mismatch", func(f *workerFixture) {
			f.edit(f.receiptPath, func(r map[string]any) { r["service"].(map[string]any)["extra"] = 1 })
		}},
		{"scope-lock-is-a-directory", "worker_policy_unreadable", func(f *workerFixture) {
			f.remove(f.scopeLock)
			if err := os.Mkdir(f.scopeLock, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"receipt-led-by-whitespace", observedPolicy, func(f *workerFixture) { f.write(f.receiptPath, "  \n"+rawText(t, f.receiptPath)) }},
		{"policy-with-nested-values", observedPolicy, func(f *workerFixture) {
			f.edit(f.receiptPath, func(r map[string]any) {
				r["policy"].(map[string]any)["extra"] = map[string]any{"b": []any{json.Number("1"), json.Number("2.5"), "x", nil, true}, "a": json.Number("10")}
			})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newWorkerFixture(t, policy)
			c.change(f)
			service, managed, servicePolicy, managedPolicy := answers(t, f)
			want := nullable(c.reason)
			if !sameReason(service, want) {
				t.Errorf("service reader answered %s, want %s", show(service), show(want))
			}
			if !sameReason(managed, want) {
				t.Errorf("managed-start's reader answered %s, want %s", show(managed), show(want))
			}
			if want != nil {
				return
			}
			// An observation hands back the receipt's policy as written.
			var receipt struct{ Policy any }
			decoder := json.NewDecoder(strings.NewReader(rawText(t, f.receiptPath)))
			decoder.UseNumber()
			if err := decoder.Decode(&receipt); err != nil {
				t.Fatal(err)
			}
			wantPolicy := normalized(t, receipt.Policy)
			if got := normalized(t, servicePolicy); !reflect.DeepEqual(got, wantPolicy) {
				t.Errorf("service reader's policy %v, want %v", got, wantPolicy)
			}
			if got := normalized(t, managedPolicy); !reflect.DeepEqual(got, wantPolicy) {
				t.Errorf("managed-start's policy %v, want %v", got, wantPolicy)
			}
		})
	}
}

// A host that refuses pidfd_open (a seccomp filter, a kernel without it) leaves the service's
// handle on the worker without a descriptor. The service holds a pidfd to the worker it observes
// and refuses one it cannot hold; managed-start's reader never held one, and answers from the
// worker's /proc entry alone, as it always did.
func TestWorkerPolicy_a_refused_pidfd_is_the_services_refusal_only(t *testing.T) {
	f := newWorkerFixture(t, workerPolicy(t))
	restore := service.SetOpenProcess(func(pid int) *service.ProcessHandle {
		return &service.ProcessHandle{PID: pid, FD: -1, Detail: "pidfd_open: function not implemented"}
	})
	defer restore()
	if got, want := observed(f.service.ReadWorkerPolicy(context.Background())), nullable("worker_policy_process_unavailable"); !sameReason(got, want) {
		t.Errorf("service reader answered %s, want %s", show(got), show(want))
	}
	if _, reason := f.observer.Read(context.Background()); reason != "" {
		t.Errorf("managed-start's reader answered %s, want observed", reason)
	}
}

// A change made after both locks were checked and before the final look (the seam is where Python
// patches service.record): each reader gets a fresh fixture, since the first to run would be the
// only one to see the original files.
func TestWorkerPolicy_changes_during_the_observation_have_one_answer_in_both_readers(t *testing.T) {
	policy := workerPolicy(t)
	for _, c := range []struct {
		name, reason string
		change       func(f *workerFixture)
	}{
		{"record-is-another-run's", "worker_policy_observation_changed", func(f *workerFixture) {
			f.edit(f.recordPath, func(r map[string]any) { r["token"] = "new-run" })
		}},
		{"database-is-replaced-by-another-file", "worker_policy_observation_changed", func(f *workerFixture) {
			database := filepath.Join(f.state, "relay.sqlite3")
			f.write(database+".copy", rawText(t, database))
			if err := os.Rename(database+".copy", database); err != nil {
				t.Fatal(err)
			}
		}},
		{"scope-claim-is-rewritten", "worker_policy_observation_changed", func(f *workerFixture) {
			f.edit(f.scopeJSON, func(r map[string]any) { r["registeredAt"] = "later" })
		}},
		// The database is gone, which the final look cannot examine: unreadable, not changed.
		{"database-is-removed", "worker_policy_unreadable", func(f *workerFixture) { f.remove(filepath.Join(f.state, "relay.sqlite3")) }},
	} {
		for _, reader := range []string{"service", "managed-start"} {
			t.Run(c.name+"/"+reader, func(t *testing.T) {
				f := newWorkerFixture(t, policy)
				calls := 0
				restore := service.SetBeforeRecheck(func() {
					calls++
					c.change(f)
				})
				defer restore()
				var got *string
				if reader == "service" {
					got = observed(f.service.ReadWorkerPolicy(context.Background()))
				} else {
					_, reason := f.observer.Read(context.Background())
					got = nullable(reason)
				}
				if calls != 1 {
					t.Fatalf("the reader reached its final look %d times, want once", calls)
				}
				if want := nullable(c.reason); !sameReason(got, want) {
					t.Errorf("answered %s, want %s", show(got), show(want))
				}
			})
		}
	}
}
