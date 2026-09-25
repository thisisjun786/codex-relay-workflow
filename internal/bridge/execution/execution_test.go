package execution

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestPolicy_whenPythonPolicyFileIsLoaded re-expresses the policy-loading block of test_execution.py.
func TestPolicy_whenPythonPolicyFileIsLoaded(t *testing.T) {
	runPython(t, []pythonCase{
		{"test_a_role_pair_the_allowlist_omits_is_refused_at_startup_not_at_creation", func(t *testing.T) {
			_, err := load(t, doc{"allowed": []any{doc{"model": supersededParent[0], "efforts": []any{supersededParent[1]}}}, "roles": doc{"parent": doc{"model": parentModel, "reasoningEffort": parentEffort}}})
			message := policyError(t, err).Error()
			for _, want := range []string{"'parent'", parentModel, parentEffort} {
				if !strings.Contains(message, want) {
					t.Fatalf("%q lacks %q", message, want)
				}
			}
		}},
		{"test_an_allowed_model_at_an_effort_the_role_needs_is_still_a_disagreement", func(t *testing.T) {
			_, err := load(t, doc{"allowed": []any{doc{"model": supersededParent[0], "efforts": []any{"high"}}}, "roles": doc{"parent": doc{"model": supersededParent[0], "reasoningEffort": supersededParent[1]}}})
			if !strings.Contains(policyError(t, err).Error(), supersededParent[1]) {
				t.Fatal(err)
			}
		}},
		{"test_a_supervisor_is_not_held_to_the_allowlist_at_load_because_it_declares_no_pair", func(t *testing.T) {
			p := mustLoad(t, doc{"allowed": []any{doc{"model": model, "efforts": []any{effort}}}, "roles": doc{"supervisor": doc{"expectation": "record"}, "child": doc{"model": model, "reasoningEffort": effort}}})
			if p.Summary()["mode"] != "allowlist" {
				t.Fatal(p.Summary())
			}
		}},
		{"test_roles_may_still_be_declared_with_no_allowlist_at_all", func(t *testing.T) {
			p := mustLoad(t, rolesPolicy(nil, false))
			if p.Summary()["mode"] != "presence_only" {
				t.Fatal(p.Summary())
			}
			authorize(t, p, Input{Model: parentModel, Effort: parentEffort, Role: "parent"})
		}},
		{"test_an_unconfigured_host_still_demands_a_stated_pair", func(t *testing.T) {
			p, err := FromEnvironment(map[string]string{})
			if err != nil {
				t.Fatal(err)
			}
			summary := p.Summary()
			if summary["mode"] != "presence_only" || summary["digest"] != nil || len(summary["roles"].(map[string]any)) != 0 {
				t.Fatal(summary)
			}
			r := refused(t, second(p.Authorize(Input{Model: nil, Effort: effort})))
			if r.Code != Missing || r.Field != "model" {
				t.Fatal(r)
			}
			authorize(t, p, Input{Model: unapproved, Effort: "low"})
		}},
		{"test_an_exception_cannot_be_invented_where_none_was_written", func(t *testing.T) {
			r := refused(t, second(Policy{}.Authorize(Input{Model: model, Effort: effort, Exception: "i-approve-this"})))
			if r.Code != ExceptionUnknown {
				t.Fatal(r)
			}
		}},
		{"test_an_unusable_policy_file_is_refused_rather_than_partly_honoured", testUnusablePolicy},
		{"test_a_missing_or_malformed_file_stops_the_policy_from_loading", func(t *testing.T) {
			dir := t.TempDir()
			if err := second(FromFile(filepath.Join(dir, "not-here.json"))); !strings.Contains(policyError(t, err).Error(), "cannot read") {
				t.Fatal(err)
			}
			broken := filepath.Join(dir, "broken.json")
			if err := os.WriteFile(broken, []byte("{ not json"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := second(FromFile(broken)); !strings.Contains(policyError(t, err).Error(), "not valid JSON") {
				t.Fatal(err)
			}
		}},
		{"test_a_repeated_key_is_refused_rather_than_silently_overwritten", func(t *testing.T) {
			dir := t.TempDir()
			rows := []struct{ body, want string }{
				{`{"allowed": [{"model": "a", "efforts": ["x"]}], "allowed": [{"model": "b", "efforts": ["y"]}]}`, "duplicate key 'allowed'"},
				{`{"allowed": [{"model": "a", "efforts": ["x"], "model": "b"}]}`, "duplicate key 'model'"},
			}
			for _, row := range rows {
				err := second(FromFile(writePolicy(t, dir, []byte(row.body))))
				if !strings.Contains(policyError(t, err).Error(), row.want) {
					t.Fatalf("%s: %v", row.body, err)
				}
			}
		}},
		{"test_the_digest_identifies_the_file_without_disclosing_it", func(t *testing.T) {
			dir := canonicalTemp(t)
			path := writePolicy(t, dir, marshal(t, policyFor(dir)))
			p, err := FromFile(path)
			if err != nil {
				t.Fatal(err)
			}
			summary := p.Summary()
			digest, _ := summary["digest"].(string)
			if summary["mode"] != "allowlist" || len(digest) != 64 {
				t.Fatal(summary)
			}
			receipt := authorize(t, p, Input{Model: model, Effort: effort}).Receipt
			text := string(marshal(t, receipt))
			if strings.Contains(text, path) || strings.Contains(text, "operator's note") || receipt["exception"] != nil || receipt["digest"] != digest {
				t.Fatal(text)
			}
		}},
		{"test_a_registered_digest_that_matches_the_file_loads_it", func(t *testing.T) {
			dir := canonicalTemp(t)
			path := writePolicy(t, dir, marshal(t, policyFor(dir)))
			first, err := FromFile(path)
			if err != nil {
				t.Fatal(err)
			}
			p, err := FromEnvironment(map[string]string{EnvPolicy: path, EnvDigest: first.digest})
			if err != nil || p.Summary()["digest"] != first.digest {
				t.Fatal(err)
			}
		}},
		{"test_a_file_that_changed_after_it_was_registered_is_refused", func(t *testing.T) {
			dir := canonicalTemp(t)
			path := writePolicy(t, dir, marshal(t, policyFor(dir)))
			registered, err := FromFile(path)
			if err != nil {
				t.Fatal(err)
			}
			writePolicy(t, dir, marshal(t, doc{"allowed": []any{doc{"model": model, "efforts": []any{effort}}}}))
			err = second(FromEnvironment(map[string]string{EnvPolicy: path, EnvDigest: registered.digest}))
			if !strings.Contains(policyError(t, err).Error(), "changed after it was registered") {
				t.Fatal(err)
			}
		}},
		{"test_a_digest_naming_no_file_is_refused_rather_than_read_as_no_policy", func(t *testing.T) {
			for _, configured := range []*string{nil, ptr(""), ptr("   ")} {
				env := map[string]string{EnvDigest: strings.Repeat("a", 64)}
				if configured != nil {
					env[EnvPolicy] = *configured
				}
				if err := second(FromEnvironment(env)); !strings.Contains(policyError(t, err).Error(), "names no file") {
					t.Fatal(err)
				}
			}
		}},
		{"test_without_a_digest_the_environment_reads_exactly_as_before", func(t *testing.T) {
			dir := canonicalTemp(t)
			path := writePolicy(t, dir, marshal(t, policyFor(dir)))
			for _, env := range []map[string]string{{}, {EnvDigest: "  "}} {
				p, err := FromEnvironment(env)
				if err != nil || p.Mode() != "presence_only" || p.digest != "" {
					t.Fatal(p, err)
				}
			}
			unpinned, err := FromEnvironment(map[string]string{EnvPolicy: path})
			if err != nil {
				t.Fatal(err)
			}
			direct, err := FromFile(path)
			if err != nil || string(marshal(t, unpinned.Summary())) != string(marshal(t, direct.Summary())) {
				t.Fatal(err)
			}
		}},
		{"test_bytes_a_caller_already_read_parse_exactly_as_the_file_does", func(t *testing.T) {
			dir := canonicalTemp(t)
			path := writePolicy(t, dir, marshal(t, policyFor(dir)))
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			fromBytes, err := FromBytes(raw, path)
			if err != nil {
				t.Fatal(err)
			}
			fromFile, err := FromFile(path)
			if err != nil || string(marshal(t, fromBytes.Summary())) != string(marshal(t, fromFile.Summary())) {
				t.Fatal(err)
			}
			if err := second(FromBytes([]byte(`{"allowed": [], "allowed": []}`), "somewhere")); !strings.Contains(policyError(t, err).Error(), "duplicate key 'allowed'") {
				t.Fatal(err)
			}
			if err := second(FromBytes([]byte("{ not json"), "somewhere")); !strings.Contains(policyError(t, err).Error(), "somewhere is not valid JSON") {
				t.Fatal(err)
			}
		}},
		{"test_a_policy_path_that_is_not_a_regular_file_is_refused_without_blocking", func(t *testing.T) {
			// Given: a FIFO nobody writes to, and a directory. A blocking open would hang here and
			// the test binary's own timeout would fail it.
			dir := t.TempDir()
			pipe := filepath.Join(dir, "policy.fifo")
			if err := syscall.Mkfifo(pipe, 0o600); err != nil {
				t.Fatal(err)
			}
			directory := filepath.Join(dir, "a-directory")
			if err := os.Mkdir(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{pipe, directory} {
				if err := second(FromFile(path)); !strings.Contains(policyError(t, err).Error(), "not a regular file") {
					t.Fatalf("%s: %v", path, err)
				}
			}
		}},
	})
}

func ptr(s string) *string { return &s }

// testUnusablePolicy is the parametrized table of
// test_an_unusable_policy_file_is_refused_rather_than_partly_honoured, one row per Python row.
func testUnusablePolicy(t *testing.T) {
	entry := func(extra doc) doc {
		e := doc{"model": model, "reasoningEffort": effort, "cwd": []any{"/tmp"}}
		for k, v := range extra {
			e[k] = v
		}
		return e
	}
	rows := []struct {
		mutation doc
		expected string
	}{
		{doc{"allowed": []any{}}, "non-empty list"},
		{doc{"allowed": []any{doc{"model": model}}}, "both model and efforts"},
		{doc{"allowed": []any{doc{"model": model, "efforts": []any{}}}}, "non-empty list of efforts"},
		{doc{"allowed": []any{doc{"model": model, "efforts": []any{effort}, "extra": 1}}}, "unknown keys"},
		{doc{"allowed": []any{doc{"model": model, "efforts": []any{effort}}, doc{"model": model, "efforts": []any{"x"}}}}, "listed twice"},
		{doc{"models": []any{model}}, "unknown keys"},
		{doc{"exceptions": doc{"e": doc{"model": model, "cwd": []any{"/tmp"}}}}, "missing"},
		{doc{"exceptions": doc{"e": entry(doc{"cwd": []any{}})}}, "at least one cwd"},
		{doc{"exceptions": doc{"e": entry(doc{"cwd": []any{"relative"}})}}, "canonical and absolute"},
		{doc{"exceptions": doc{"e": entry(doc{"cwd": []any{"/srv/checkouts/../task"}})}}, "canonical and absolute"},
		{doc{"exceptions": doc{"e": entry(doc{"cwd": []any{"/srv/x/"}})}}, "canonical and absolute"},
		{doc{"exceptions": doc{"e": entry(doc{"cwd": []any{"/srv/\x00/task"}})}}, "cannot be resolved"},
		{doc{"exceptions": doc{strings.Repeat("x", 129): entry(doc{"cwd": []any{"/srv/task"}})}}, "an exception id must be a non-empty string"},
		{doc{"exceptions": doc{"e": entry(doc{"models": []any{unapproved}})}}, "unknown keys"},
	}
	dir := t.TempDir()
	for _, row := range rows {
		document := doc{"allowed": []any{doc{"model": model, "efforts": []any{effort}}}}
		for k, v := range row.mutation {
			document[k] = v
		}
		raw, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		err = second(FromEnvironment(map[string]string{EnvPolicy: writePolicy(t, dir, raw)}))
		if !strings.Contains(policyError(t, err).Error(), row.expected) {
			t.Errorf("%v: want %q, got %v", row.mutation, row.expected, err)
		}
	}
}
