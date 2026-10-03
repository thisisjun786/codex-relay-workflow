package recall

import "errors"

func hitCountRef(string, string) string                { return "" }
func readHitCounts(*RwDb, []string) map[string]float64 { return map[string]float64{} }
func bumpHitCounts(*RwDb, []string, string) error      { return errors.New("hit-count port pending") }
