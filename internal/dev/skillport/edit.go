//go:build dev

package skillport

import "errors"

func splitLines(text string) []string { return []string{text} }
func diffLines(a, b []string) []Hunk  { return nil }
func revert(staged []string, hunks []Hunk) ([]string, error) {
	return nil, errors.New("not implemented")
}
func RecordEdits(root string, src Source, name, reason string) (int, error) {
	return 0, errors.New("not implemented")
}
func save(root, name string, s *Skill) error { return errors.New("not implemented") }
