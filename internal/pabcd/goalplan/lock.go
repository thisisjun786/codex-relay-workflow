package goalplan

import "path/filepath"

type GoalplanWriteLockOptions struct {
	RetryDelaysMs []int
	Sleep         func(int)
	Now           func() string
}
type GoalplanWriteLockResult[T any] struct {
	Kind   string `json:"kind"`
	Value  *T     `json:"value,omitempty"`
	Reason string `json:"reason,omitempty"`
}
type GoalplanLockStatus struct {
	Path   string   `json:"path"`
	Exists bool     `json:"exists"`
	AgeMs  *float64 `json:"ageMs"`
}
type GoalplanLockStatusOptions struct {
	NowMs *float64
	Stat  func(string) (float64, error)
}

func GoalplanWriteLockDir(cwd, slug string) (string, error) {
	dir, e := GoalplanDir(cwd, slug)
	return filepath.Join(dir, GoalplanLockDir), e
}
func GoalplanWriteLockStatus(cwd, slug string, o *GoalplanLockStatusOptions) (GoalplanLockStatus, error) {
	p, e := GoalplanWriteLockDir(cwd, slug)
	return GoalplanLockStatus{Path: p}, e
}
func WithGoalplanWriteLock[T any](cwd, slug string, fn func(*Goalplan) (T, error), o *GoalplanWriteLockOptions) (GoalplanWriteLockResult[T], error) {
	p := ReadGoalplan(cwd, slug)
	if p == nil {
		return GoalplanWriteLockResult[T]{Kind: "unreadable"}, nil
	}
	v, e := fn(p)
	return GoalplanWriteLockResult[T]{Kind: "ok", Value: &v}, e
}
