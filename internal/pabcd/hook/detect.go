// Package hook contains the PABCD prompt detectors ported from CXC v0.2.40.
package hook

import "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"

func DetectTrigger(prompt string) (state.Phase, bool) { return "", false }
func DetectLoopArmRequest(prompt string) bool         { return false }
func DetectAgbrowseSearchRequest(prompt string) bool  { return false }
func DetectMemoryWriteRequest(prompt string) bool     { return false }
func requestLines(prompt string) []string             { return nil }
