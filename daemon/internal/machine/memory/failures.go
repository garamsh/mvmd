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
	holds map[string]heldCall
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

// Hold makes the next call of the step wait, as a slow VMM does. entered is
// closed when that call is waiting; release lets it go on, to the failure the
// step has by then or to success.
func (f *Failures) Hold(step string) (entered <-chan struct{}, release func()) {
	hold, ch := make(chan struct{}), make(chan struct{})
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.holds == nil {
		f.holds = make(map[string]heldCall)
	}
	f.holds[step] = heldCall{hold: hold, entered: ch}
	return ch, func() { close(hold) }
}

type heldCall struct {
	hold    chan struct{}
	entered chan struct{}
}

// Enter counts a call of the step and returns the failure it was told to have.
func (f *Failures) Enter(step string) error {
	f.mu.Lock()
	h, held := f.holds[step]
	delete(f.holds, step)
	f.mu.Unlock()
	if held {
		close(h.entered)
		<-h.hold
	}

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
