// Copyright (c) 2026 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package dig_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/dig"
)

// eventLog is an in-memory record of cleanup events, used to assert on the
// order and ownership of resource cleanup.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *eventLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

// cleanup returns a CleanupCallback that records an event and returns the
// given error.
func (l *eventLog) cleanup(name string, err error) dig.CleanupCallback {
	return func() error {
		l.add("cleanup " + name)
		return err
	}
}

func TestCleanupOrder(t *testing.T) {
	t.Parallel()

	type A struct{}
	type B struct{}
	type C struct{}
	type D struct{}

	log := &eventLog{}
	c := dig.New()

	// A <- B <- C, and D independent of the chain.
	require.NoError(t, c.Provide(func() *A { return &A{} }, dig.WithCleanupCallback(log.cleanup("A", nil))))
	require.NoError(t, c.Provide(func(*A) *B { return &B{} }, dig.WithCleanupCallback(log.cleanup("B", nil))))
	require.NoError(t, c.Provide(func(*B) *C { return &C{} }, dig.WithCleanupCallback(log.cleanup("C", nil))))
	require.NoError(t, c.Provide(func() *D { return &D{} }, dig.WithCleanupCallback(log.cleanup("D", nil))))

	require.NoError(t, c.Invoke(func(*C, *D) {}))
	require.NoError(t, c.Close())

	// Dependents are cleaned up before their dependencies; independent
	// values are cleaned up in a stable reverse-construction order.
	assert.Equal(t, []string{
		"cleanup D",
		"cleanup C",
		"cleanup B",
		"cleanup A",
	}, log.all())
}

func TestCleanupConstructorFailure(t *testing.T) {
	t.Parallel()

	type A struct{}
	type B struct{}

	log := &eventLog{}
	c := dig.New()

	require.NoError(t, c.Provide(func() *A { return &A{} }, dig.WithCleanupCallback(log.cleanup("A", nil))))
	require.NoError(t, c.Provide(
		func(*A) (*B, error) { return nil, errors.New("great sadness") },
		dig.WithCleanupCallback(log.cleanup("B", nil)),
	))

	err := c.Invoke(func(*B) {})
	require.Error(t, err, "constructor B fails midway through resolution")

	require.NoError(t, c.Close())

	// Only the successfully constructed A is cleaned up; the failed
	// constructor B never registered its cleanup callback.
	assert.Equal(t, []string{"cleanup A"}, log.all())
}

func TestCleanupUnusedProvider(t *testing.T) {
	t.Parallel()

	type A struct{}

	log := &eventLog{}
	c := dig.New()

	require.NoError(t, c.Provide(func() *A { return &A{} }, dig.WithCleanupCallback(log.cleanup("A", nil))))

	require.NoError(t, c.Close())

	// The provider was never used, so nothing was created and nothing is
	// cleaned up.
	assert.Empty(t, log.all())
}

func TestCleanupScopeOwnership(t *testing.T) {
	t.Parallel()

	type shared struct{}
	type local1 struct{}
	type local2 struct{}

	log := &eventLog{}
	c := dig.New()

	// Owned by the root container; inherited by both child scopes.
	require.NoError(t, c.Provide(func() *shared { return &shared{} },
		dig.WithCleanupCallback(log.cleanup("shared", nil))))

	child1 := c.Scope("child1")
	child2 := c.Scope("child2")

	require.NoError(t, child1.Provide(func(*shared) *local1 { return &local1{} },
		dig.WithCleanupCallback(log.cleanup("local1", nil))))
	require.NoError(t, child2.Provide(func(*shared) *local2 { return &local2{} },
		dig.WithCleanupCallback(log.cleanup("local2", nil))))

	require.NoError(t, child1.Invoke(func(*local1) {}))
	require.NoError(t, child2.Invoke(func(*local2) {}))

	// Closing child1 releases only what child1 owns; the inherited parent
	// instance stays alive.
	require.NoError(t, child1.Close())
	assert.Equal(t, []string{"cleanup local1"}, log.all())

	// Closing the container closes the still-open child2 first, then
	// releases the shared instance exactly once.
	require.NoError(t, c.Close())
	assert.Equal(t, []string{
		"cleanup local1",
		"cleanup local2",
		"cleanup shared",
	}, log.all())
}

func TestCleanupCallbackErrors(t *testing.T) {
	t.Parallel()

	type A struct{}
	type B struct{}
	type C struct{}

	errB := errors.New("B cleanup failed")
	errC := errors.New("C cleanup failed")

	log := &eventLog{}
	c := dig.New()

	require.NoError(t, c.Provide(func() *A { return &A{} }, dig.WithCleanupCallback(log.cleanup("A", nil))))
	require.NoError(t, c.Provide(func() *B { return &B{} }, dig.WithCleanupCallback(log.cleanup("B", errB))))
	require.NoError(t, c.Provide(func() *C { return &C{} }, dig.WithCleanupCallback(log.cleanup("C", errC))))

	require.NoError(t, c.Invoke(func(*A, *B, *C) {}))

	err := c.Close()
	require.Error(t, err)

	// A failing callback does not stop the remaining callbacks, and the
	// container does not forget the other created resources.
	assert.Equal(t, []string{
		"cleanup C",
		"cleanup B",
		"cleanup A",
	}, log.all())

	// All callback errors are preserved.
	assert.ErrorIs(t, err, errB)
	assert.ErrorIs(t, err, errC)

	// A repeated Close reports the same state without re-running anything.
	assert.Equal(t, err, c.Close())
	assert.Len(t, log.all(), 3)
}

func TestCleanupMultipleOutputs(t *testing.T) {
	t.Parallel()

	type A struct{}
	type B struct{}

	log := &eventLog{}
	c := dig.New()

	// One constructor producing multiple outputs registers a single cleanup.
	require.NoError(t, c.Provide(func() (*A, *B) { return &A{}, &B{} },
		dig.WithCleanupCallback(log.cleanup("AB", nil))))

	require.NoError(t, c.Invoke(func(*A, *B) {}))
	require.NoError(t, c.Close())

	assert.Equal(t, []string{"cleanup AB"}, log.all())
}

func TestInvokeAfterClose(t *testing.T) {
	t.Parallel()

	type A struct{}

	c := dig.New()
	require.NoError(t, c.Provide(func() *A { return &A{} }))
	require.NoError(t, c.Close())

	err := c.Invoke(func(*A) {})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "closed")

	// A child scope of a closed container cannot resolve either.
	child := c.Scope("child")
	err = child.Invoke(func(*A) {})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "closed")
}

func TestCloseRepeatedWithoutErrors(t *testing.T) {
	t.Parallel()

	type A struct{}

	log := &eventLog{}
	c := dig.New()
	require.NoError(t, c.Provide(func() *A { return &A{} }, dig.WithCleanupCallback(log.cleanup("A", nil))))
	require.NoError(t, c.Invoke(func(*A) {}))

	require.NoError(t, c.Close())
	require.NoError(t, c.Close())

	// Cleanups ran exactly once.
	assert.Equal(t, []string{"cleanup A"}, log.all())
}

// closerResource implements io.Closer; dig must not pick it up implicitly.
type closerResource struct {
	closed *bool
}

func (r *closerResource) Close() error {
	*r.closed = true
	return nil
}

func TestNoImplicitCloserCleanup(t *testing.T) {
	t.Parallel()

	closed := false
	c := dig.New()
	require.NoError(t, c.Provide(func() *closerResource {
		return &closerResource{closed: &closed}
	}))
	require.NoError(t, c.Invoke(func(*closerResource) {}))

	require.NoError(t, c.Close())

	// The container does not take over resources merely because they
	// happen to implement io.Closer.
	assert.False(t, closed, "io.Closer must not be called without WithCleanupCallback")
}

func TestCleanupErrorIdentifiesConstructor(t *testing.T) {
	t.Parallel()

	type A struct{}

	c := dig.New()
	require.NoError(t, c.Provide(func() *A { return &A{} },
		dig.WithCleanupCallback(func() error { return fmt.Errorf("boom") })))
	require.NoError(t, c.Invoke(func(*A) {}))

	err := c.Close()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
	assert.Contains(t, err.Error(), "cleanup")
}
