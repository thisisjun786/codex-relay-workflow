package argparse

import (
	"fmt"
	"math/big"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// IntegerValue converts Go defaults and already-parsed values, never source text.
func IntegerValue(v any) *big.Int {
	switch n := v.(type) {
	case *big.Int:
		return n
	case int:
		return big.NewInt(int64(n))
	case int64:
		return big.NewInt(n)
	}
	panic(fmt.Sprintf("integer action has value %T", v))
}

// IntegerOverflow preserves the Python host exception through transaction wraps.
type IntegerOverflow = store.IntegerOverflow

// SQLiteInteger is the same narrowing boundary as sqlite3's integer binding.
func SQLiteInteger(n *big.Int) (int64, error) {
	if !n.IsInt64() {
		return 0, &IntegerOverflow{}
	}
	return n.Int64(), nil
}
