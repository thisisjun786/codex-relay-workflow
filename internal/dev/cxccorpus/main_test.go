//go:build dev

package cxccorpus

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/homeguard"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

func TestMain(m *testing.M) {
	testsupport.Main(m, homeguard.RefuseAccountHome)
}
