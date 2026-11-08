// Package memory holds the in-memory substitutes for the ports of package
// machine. They serve tests and development; each can be told to fail at a
// named step, so the state-after-failure columns of runtime-api.md can be
// tested.
package memory

import "sync"

// Failures makes the named steps of a substitute fail, and counts the calls
// each step received. The zero value fails nothing.
type Failures struct {
	mu    sync.Mutex
	steps map[string]error
	after map[string]int
	calls map[string]int
}

// FailAt makes every later call of the step return err.
func (f *Failures) FailAt(step string, err error) {
	f.FailAfter(step, f.Calls(step), err)
}

// FailAfter lets the step's calls up to the n-th (counting from the first
// ever) succeed and makes every later one return err.
func (f *Failures) FailAfter(step string, n int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.steps == nil {
		f.steps = make(map[string]error)
		f.after = make(map[string]int)
	}
	f.steps[step] = err
	f.after[step] = n
}

// Heal stops the step from failing.
func (f *Failures) Heal(step string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.steps, step)
	delete(f.after, step)
}

// Calls returns how many times the step was called, failed or not.
func (f *Failures) Calls(step string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[step]
}

// enter counts a call of the step and returns the failure it was told to have.
func (f *Failures) enter(step string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = make(map[string]int)
	}
	f.calls[step]++
	if f.calls[step] <= f.after[step] {
		return nil
	}
	return f.steps[step]
}
