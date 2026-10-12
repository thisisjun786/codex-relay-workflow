// Package scenario is the scenario harness of the flow engine: it runs the built-in workflows (internal/flow/workflows) end to
// end on the engine, over the fake App Server host (internal/bridge/appserver/fakehost), a temporary relay store, a temporary git
// repository and a scripted model, and asserts on what the run did - the calls it made in order, the rows it journaled, the
// decisions it recorded, the commits it landed, and what a resume does and does not run again. Nothing here touches the real
// host, the real relay state or the network.
//
// The harness itself lives in the _test.go files of this package so that every issue that adds a workflow adds its scenarios
// beside the others and shares one lab (lab_test.go); this file only gives the package a name for `go build`.
package scenario
