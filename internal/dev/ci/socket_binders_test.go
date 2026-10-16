//go:build dev

package ci

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// socketBinderPattern finds the test code that binds, dials or names a unix socket: a listen, a
// bind address, a fakehost socket or a socket name. It is written so that this file does not match
// it.
const socketBinderPattern = `net[.]Listen[(]"unix"|ListenUnix[(]|DialUnix[(]|SockaddrUnix[{]|fakehost[.]SocketPath[(]|[.]sock"`

// socketBindPattern is the part of socketBinderPattern that binds or dials.
var socketBindPattern = regexp.MustCompile(`net[.]Listen[(]"unix"|ListenUnix[(]|DialUnix[(]|SockaddrUnix[{]`)

// socketMeasurePattern is a measure of the socket path: tmploctest's SocketPath or CheckSocket, or
// fakehost's SocketPath, which is tmploctest.SocketPath.
var socketMeasurePattern = regexp.MustCompile(`tmploctest[.](SocketPath|CheckSocket)[(]|fakehost[.]SocketPath[(]`)

// socketBinderPaths are the files the check reads: every test file, and the test-support packages
// that are not test files (contracttest, testsupport, fakehost).
var socketBinderPaths = []string{"*_test.go", "internal/contracttest/*.go", "internal/testsupport/*.go", "internal/bridge/appserver/fakehost/*.go"}

// socketException is a file that binds, dials or names a unix socket without a measure, and why.
// binds says the file may bind or dial; a names-only exception that starts to bind fails the check.
type socketException struct {
	binds  bool
	reason string
}

const (
	socketNamedOnly = "names a socket path that nothing in the file binds or dials: a --socket argument, a store's socket " +
		"scope, a recorded field or a configured value"
	socketHelperBinds = "names the control socket the package's host helper (harness_test.go newHost) binds after " +
		"tmploctest.CheckSocket has measured it; this file only removes it"
	socketProductAlias = "binds or dials the product's control socket through hook.ControlAddress, which reaches a path " +
		"over the bound through a /proc/self/fd alias; the product's socket rule is out of scope (CRW-1568)"
	socketOverTheBound = "names a control socket over the bound on purpose: the hook's unreachable row for a path a unix " +
		"socket cannot hold (CRW-1393); nothing binds it"
	socketHelpersUnderTest = "tmploctest's own tests: the helpers under test bind the paths they return"
)

// socketBinderExceptions is the socket inventory's (CRW-1568) list of
// files that bind, dial or name a unix socket and measure none. A file not listed must measure its
// socket path; a listed file that measures one, or no longer has a socket, is a stale entry.
var socketBinderExceptions = map[string]socketException{
	"internal/dev/stopevents/exclusions_test.go":                {false, socketHelperBinds},
	"internal/dev/stopevents/runtime_build_test.go":             {false, socketHelperBinds},
	"internal/dev/stopevents/stopevents_test.go":                {false, socketHelperBinds},
	"internal/relay/adapter/child_emit_linux_test.go":           {true, socketProductAlias},
	"internal/relay/adapter/managed_marker_root_daemon_test.go": {true, socketProductAlias},
	"internal/relay/hook/crw1393_socket_path_test.go":           {false, socketOverTheBound},
	"internal/tmploc/tmploctest/tmploctest_test.go":             {true, socketHelpersUnderTest},
}

// socketNamedOnlyFiles are the names-only exceptions (socketNamedOnly).
var socketNamedOnlyFiles = []string{
	"cmd/crw/crw1284_child_push_test.go",
	"cmd/crw/crw1303_subagent_push_test.go",
	"cmd/crw/flow_cli_dispatch_test.go",
	"cmd/crw/relay_state_config_test.go",
	"internal/bridge/ledger/alias_test.go",
	"internal/bridge/mcp/validate_test.go",
	"internal/bridge/mcp/wire_test.go",
	"internal/bridge/owned_retry_test.go",
	"internal/contracttest/mcp.go",
	"internal/contracttest/mcpnorm_test.go",
	"internal/crwconfig/check_test.go",
	"internal/crwconfig/placement_test.go",
	"internal/flow/cli/process_test.go",
	"internal/flow/cli/session_pairs_test.go",
	"internal/flow/cli/session_test.go",
	"internal/flow/compile/keywarning_test.go",
	"internal/flow/planinput/planinput_test.go",
	"internal/gui/status_api_test.go",
	"internal/manage/capacity_branches_test.go",
	"internal/manage/capacity_review779_test.go",
	"internal/manage/checkpoint_test.go",
	"internal/manage/config_storage_test.go",
	"internal/manage/config_test.go",
	"internal/manage/dagreview_store_test.go",
	"internal/manage/hostread_test.go",
	"internal/manage/pump_signals_round7_test.go",
	"internal/manage/relayread_review766_test.go",
	"internal/manage/sandboxroot_test.go",
	"internal/manage/upgrade_test.go",
	"internal/pluginwiring/launch_test.go",
	"internal/pluginwiring/wiring_test.go",
	"internal/policystore/applied_test.go",
	"internal/policystore/running_test.go",
	"internal/relay/adapter/daemon_sweep_test.go",
	"internal/relay/adapter/emit_confirm_turn_test.go",
	"internal/relay/adapter/factory_test.go",
	"internal/relay/adapter/ledger_test.go",
	"internal/relay/adapter/worker_scope_test.go",
	"internal/relay/cli/doctor_ownership_test.go",
	"internal/relay/cli/doctor_properties_test.go",
	"internal/relay/cli/doctor_service_store_test.go",
	"internal/relay/cli/fixture_support_test.go",
	"internal/relay/cli/pycli_helpers_test.go",
	"internal/relay/cli/readonly_test.go",
	"internal/relay/cli/receive_test.go",
	"internal/relay/cli/reporting_selector_order_test.go",
	"internal/relay/cli/state_path_parity_test.go",
	"internal/relay/cli/unencodable_argument_test.go",
	"internal/relay/dagsched/agent_release_test.go",
	"internal/relay/dagsched/cli_test.go",
	"internal/relay/dagsched/helpers_test.go",
	"internal/relay/dagsched/release_refusals_test.go",
	"internal/relay/delivery/cli_emit_no_ledger_test.go",
	"internal/relay/delivery/cli_emit_turn_id_check_test.go",
	"internal/relay/delivery/receive_test.go",
	"internal/relay/delivery/selection_test.go",
	"internal/relay/delivery/turn_status_refusal_test.go",
	"internal/relay/hook/childbinding_test.go",
	"internal/relay/hook/crw1303_subagent_binding_test.go",
	"internal/relay/hook/journal_row_test.go",
	"internal/relay/hook/stop_read_test.go",
	"internal/relay/registry/assignment_test.go",
	"internal/relay/registry/child_sandbox_test.go",
	"internal/relay/selection/recovery_parity_test.go",
	"internal/relay/service/launch_surfaces_test.go",
	"internal/relay/service/sandboxroot_launch_test.go",
	"internal/relay/service/scope_root_test.go",
	"internal/relay/service/worker_reasons_test.go",
	"internal/relay/store/create_race_test.go",
	"internal/relay/store/creation_gate_test.go",
	"internal/relay/store/default_socket_scope_test.go",
	"internal/relay/store/open_cost_test.go",
	"internal/relay/store/ownership/protocol_test.go",
	"internal/relay/store/ownership_test.go",
	"internal/relay/store/pathlib_parity_test.go",
	"internal/relay/store/python_compat_test.go",
	"internal/relay/store/socket_binding_test.go",
	"internal/relay/store/state_config_test.go",
	"internal/relay/store/state_selection_test.go",
	"internal/relay/store/state_test.go",
	"internal/relay/store/store_test.go",
	"internal/relay/supervisor/eventless_guard_test.go",
	"internal/relay/supervisor/layer_words_test.go",
	"internal/relay/supervisor/rcl_test.go",
	"internal/relay/supervisor/reporting_cli_test.go",
	"internal/relay/supervisor/send_test.go",
	"internal/relay/supervisor/wording_ids_test.go",
	"internal/role/dispatch_host_test.go",
	"internal/runtime/install/lifecycle_test.go",
	"internal/runtime/install/outgoing_test.go",
	"internal/runtime/install/unit_test.go",
	"internal/runtime/install/unit_workdir_internal_test.go",
	"internal/runtime/integration/isolated_home_test.go",
	"internal/testsupport/ownership.go",
	"internal/testsupport/ownership_test.go",
}

// socketExceptions is socketBinderExceptions with the names-only files added.
func socketExceptions(t *testing.T) map[string]socketException {
	t.Helper()
	all := map[string]socketException{}
	for file, e := range socketBinderExceptions {
		all[file] = e
	}
	for _, file := range socketNamedOnlyFiles {
		if _, twice := all[file]; twice {
			t.Errorf("%s is listed twice in the socket binder exceptions", file)
		}
		all[file] = socketException{false, socketNamedOnly}
	}
	return all
}

// CRW-1568 (R1-8): every test file that binds, dials or names a unix socket measures the path
// against tmploc.SocketTailMax (tmploctest.SocketPath, or tmploctest.CheckSocket right before the
// bind), or is a listed exception with its reason. A new binder that composes its own path cannot
// skip the measure, and a stale exception fails as the main check's do.
func TestSocketBindersCheckTheirTail(t *testing.T) {
	root := repoRoot()
	out, err := runGit(root, append([]string{"grep", "-l", "-I", "-E", socketBinderPattern, "--"}, socketBinderPaths...)...)
	if err != nil {
		t.Fatal(err)
	}
	files := strings.Fields(string(out))
	if len(files) == 0 {
		t.Fatal("no test file binds or names a unix socket: the pattern finds nothing")
	}
	exceptions := socketExceptions(t)
	hit := map[string]bool{}
	lineOf := regexp.MustCompile(socketBinderPattern)
	for _, file := range files {
		hit[file] = true
		raw, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			t.Fatal(err)
		}
		measured := socketMeasurePattern.Match(raw)
		e, excepted := exceptions[file]
		switch {
		case measured && excepted:
			t.Errorf("%s measures its socket path now: remove its stale entry from the socket binder exceptions", file)
		case !measured && !excepted:
			t.Errorf("%s binds, dials or names a unix socket (%s) and measures none: call tmploctest.SocketPath, or tmploctest.CheckSocket before the bind, or list it in the socket binder exceptions with a reason",
				file, socketLine(raw, lineOf))
		case excepted && !e.binds && socketBindPattern.Match(raw):
			t.Errorf("%s binds or dials a unix socket (%s) but its exception is names-only: measure the path with tmploctest.CheckSocket before the bind",
				file, firstLine(raw, socketBindPattern))
		}
	}
	stale := []string{}
	for file, e := range exceptions {
		if strings.TrimSpace(e.reason) == "" {
			t.Errorf("%s: the socket binder exception has no reason", file)
		}
		if !hit[file] {
			stale = append(stale, file)
		}
	}
	slices.Sort(stale)
	for _, file := range stale {
		t.Errorf("%s no longer binds, dials or names a unix socket: remove its stale entry from the socket binder exceptions", file)
	}
}

// socketLine is the first line of raw that binds or dials, else the first that pattern matches,
// with its number.
func socketLine(raw []byte, pattern *regexp.Regexp) string {
	if socketBindPattern.Match(raw) {
		return firstLine(raw, socketBindPattern)
	}
	return firstLine(raw, pattern)
}

// firstLine is the first line of raw that pattern matches, with its number.
func firstLine(raw []byte, pattern *regexp.Regexp) string {
	for i, line := range strings.Split(string(raw), "\n") {
		if pattern.MatchString(line) {
			return strings.TrimSpace(line) + " at line " + strconv.Itoa(i+1)
		}
	}
	return "no line"
}
