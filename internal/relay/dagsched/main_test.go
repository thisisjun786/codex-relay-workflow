package dagsched

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

func TestMain(m *testing.M) {
	testsupport.Main(m, func(root string) (func() error, error) { templateRoot = root; return nil, nil })
}
