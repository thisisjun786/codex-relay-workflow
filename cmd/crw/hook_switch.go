package main

import (
	"slices"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
	"github.com/thisisjun786/codex-relay-workflow/internal/hookswitch"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// hookSwitchTestState is the hook switch's optional link-time state, crw or cxc. Nothing in this
// repository initializes it for a release: such a build leaves it empty and reads the switch file.
// Only the build the cxc corpus replays against links crw in (-X main.hookSwitchTestState=crw,
// internal/contracttest registry.go), because the corpus' homes have no switch file and observe
// every file a hook leaves in them (contract/notes/cxc/README.md "Seams the replay does not provide").
var hookSwitchTestState string

// legArg is the leg crw hook <event> --leg <leg> (or --leg=<leg>) names, else empty.
func legArg(args []string) string {
	switch {
	case len(args) == 3 && args[1] == "--leg":
		return args[2]
	case len(args) == 2 && strings.HasPrefix(args[1], "--leg="):
		return strings.TrimPrefix(args[1], "--leg=")
	}
	return ""
}

// switchedLeg is whether the arguments name a leg behind the switch: a component row (the K1
// component legs and the GitHub post guard) or a pabcd-state leg, for the event it is registered
// for. Anything else (--plugin-launch, an unknown leg) is not behind it.
func switchedLeg(args []string, rows []componentHook) (string, bool) {
	id := legArg(args)
	if id == "" {
		return "", false
	}
	if slices.ContainsFunc(rows, func(r componentHook) bool { return r.ID == id && r.Event == args[0] }) ||
		slices.ContainsFunc(harness.Legs(), func(l harness.Leg) bool { return l.ID == id && l.Event == args[0] }) {
		return id, true
	}
	return "", false
}

// hookSwitchOn reads the switch for leg (internal/hookswitch): off ends the leg in silence before
// its input is read, and a switch that cannot be read runs the leg and leaves a warning.
func hookSwitchOn(leg string, env host.LookupEnv) bool {
	if hookSwitchTestState != "" {
		return hookSwitchTestState == hookswitch.CRW
	}
	r := hookswitch.Read(env)
	r.Warn(leg, time.Now())
	return r.On
}
