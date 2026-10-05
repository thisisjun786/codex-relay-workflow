package spawn

import (
	"os"
	"time"
)

// Compile stub for the red run: every function refuses. The port replaces it.
const spawnGrantToken = "CRW-SUBSPAWN-ALLOWED"
const spawnGrantMarker = "[CRW-SUBSPAWN-GRANT:"
const spawnGrantTTL = 15 * time.Minute

func IsSubagentSpawner(map[string]any) bool                                 { return false }
func MintRecursionGrant(map[string]any, string, time.Time) (string, bool)   { return "", false }
func ConsumeRecursionGrant(map[string]any, string, string, time.Time) bool  { return false }
func spawnGrantMint(map[string]any, string, int, time.Time) (string, bool)  { return "", false }
func spawnGrantConsume(map[string]any, string, string, int, time.Time) bool { return false }
func spawnGrantWrite(*os.Root, string, time.Time) bool                      { return false }
func spawnGrantKey(map[string]any) (string, bool)                           { return "", false }
func spawnGrantScope(map[string]any) (string, string, bool)                 { return "", "", false }
func spawnGrantDirName(int) string                                          { return "" }
func spawnGrantFile(string) string                                          { return "" }
