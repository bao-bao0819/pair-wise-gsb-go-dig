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

package dig

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// eventLog is an in-memory log of cleanup events used to assert on the
// order and ownership of resource cleanup.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) log(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

// cleanup returns a CleanupCallback that records the named event and
// returns the given error, if any.
func (l *eventLog) cleanup(name string, err error) CleanupCallback {
	return func() error {
		l.log("cleanup " + name)
		return err
	}
}

func (l *eventLog) recorded() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

func TestCleanupOrder(t *testing.T) {
	t.Run("dependents cleaned before dependencies", func(t *testing.T) {
		log := new(eventLog)
		c := New()

		type A struct{}
		type B struct{}
		type C struct{}

		require.NoError(t, c.Provide(func() *A { return &A{} },
			WithProviderCleanup(log.cleanup("A", nil))))
		require.NoError(t, c.Provide(func(*A) *B { return &B{} },
			WithProviderCleanup(log.cleanup("B", nil))))
		require.NoError(t, c.Provide(func(*B) *C { return &C{} },
			WithProviderCleanup(log.cleanup("C", nil))))

		require.NoError(t, c.Invoke(func(*C) {}))
		require.NoError(t, c.Close())

		assert.Equal(t, []string{"cleanup C", "cleanup B", "cleanup A"}, log.recorded())
	})

	t.Run("independent nodes cleaned in stable order", func(t *testing.T) {
		log := new(eventLog)
		c := New()

		type A struct{}
		type B struct{}

		require.NoError(t, c.Provide(func() *A { return &A{} },
			WithProviderCleanup(log.cleanup("A", nil))))
		require.NoError(t, c.Provide(func() *B { return &B{} },
			WithProviderCleanup(log.cleanup("B", nil))))

		// Build A before B so creation order is deterministic.
		require.NoError(t, c.Invoke(func(*A) {}))
		require.NoError(t, c.Invoke(func(*B) {}))
		require.NoError(t, c.Close())

		assert.Equal(t, []string{"cleanup B", "cleanup A"}, log.recorded())
	})
}

func TestCleanupOnlyCreatedInstances(t *testing.T) {
	t.Run("constructor fails halfway", func(t *testing.T) {
		log := new(eventLog)
		c := New()

		type A struct{}
		type B struct{}

		require.NoError(t, c.Provide(func() *A { return &A{} },
			WithProviderCleanup(log.cleanup("A", nil))))
		require.NoError(t, c.Provide(func(*A) (*B, error) {
			return nil, errors.New("great sadness")
		}, WithProviderCleanup(log.cleanup("B", nil))))

		err := c.Invoke(func(*B) {})
		require.Error(t, err)

		require.NoError(t, c.Close())
		assert.Equal(t, []string{"cleanup A"}, log.recorded(),
			"failed constructor must not register its cleanup")
	})

	t.Run("unused provider is not cleaned", func(t *testing.T) {
		log := new(eventLog)
		c := New()

		type A struct{}
		type B struct{}

		require.NoError(t, c.Provide(func() *A { return &A{} },
			WithProviderCleanup(log.cleanup("A", nil))))
		require.NoError(t, c.Provide(func() *B { return &B{} },
			WithProviderCleanup(log.cleanup("B", nil))))

		require.NoError(t, c.Invoke(func(*A) {}))
		require.NoError(t, c.Close())

		assert.Equal(t, []string{"cleanup A"}, log.recorded(),
			"provider that never ran must not be cleaned")
	})

	t.Run("multiple outputs register cleanup once", func(t *testing.T) {
		log := new(eventLog)
		c := New()

		type out struct {
			Out

			A *int
			B *string
		}

		require.NoError(t, c.Provide(func() out {
			i, s := 42, "foo"
			return out{A: &i, B: &s}
		}, WithProviderCleanup(log.cleanup("out", nil))))

		require.NoError(t, c.Invoke(func(*int, *string) {}))
		require.NoError(t, c.Close())

		assert.Equal(t, []string{"cleanup out"}, log.recorded())
	})

	t.Run("io.Closer is not closed implicitly", func(t *testing.T) {
		log := new(eventLog)
		c := New()

		require.NoError(t, c.Provide(func() *closerResource {
			return &closerResource{log: log}
		}))

		require.NoError(t, c.Invoke(func(*closerResource) {}))
		require.NoError(t, c.Close())

		assert.Empty(t, log.recorded(),
			"values implementing io.Closer must not be closed without an explicit cleanup")
	})
}

// closerResource implements io.Closer but is provided without
// WithProviderCleanup.
type closerResource struct {
	log *eventLog
}

func (r *closerResource) Close() error {
	r.log.log("close closerResource")
	return nil
}

func TestCleanupScopeOwnership(t *testing.T) {
	t.Run("child close does not release parent instance", func(t *testing.T) {
		log := new(eventLog)
		c := New()

		type shared struct{}
		type childRes struct{}

		require.NoError(t, c.Provide(func() *shared { return &shared{} },
			WithProviderCleanup(log.cleanup("shared", nil))))

		child := c.Scope("child")
		require.NoError(t, child.Provide(func(*shared) *childRes { return &childRes{} },
			WithProviderCleanup(log.cleanup("childRes", nil))))

		require.NoError(t, child.Invoke(func(*childRes) {}))

		require.NoError(t, child.Close())
		assert.Equal(t, []string{"cleanup childRes"}, log.recorded(),
			"closing the child must not release the parent's instance")

		require.NoError(t, c.Close())
		assert.Equal(t, []string{"cleanup childRes", "cleanup shared"}, log.recorded())
	})

	t.Run("parent close closes surviving children first", func(t *testing.T) {
		log := new(eventLog)
		c := New()

		type shared struct{}
		type res1 struct{}
		type res2 struct{}

		require.NoError(t, c.Provide(func() *shared { return &shared{} },
			WithProviderCleanup(log.cleanup("shared", nil))))

		child1 := c.Scope("child1")
		child2 := c.Scope("child2")

		require.NoError(t, child1.Provide(func(*shared) *res1 { return &res1{} },
			WithProviderCleanup(log.cleanup("res1", nil))))
		require.NoError(t, child2.Provide(func(*shared) *res2 { return &res2{} },
			WithProviderCleanup(log.cleanup("res2", nil))))

		// Both siblings resolve the shared parent instance.
		require.NoError(t, child1.Invoke(func(*res1) {}))
		require.NoError(t, child2.Invoke(func(*res2) {}))

		// Close one sibling explicitly; the other is still alive.
		require.NoError(t, child1.Close())
		assert.Equal(t, []string{"cleanup res1"}, log.recorded())

		// Closing the parent closes the surviving child first, then
		// releases its own instance exactly once.
		require.NoError(t, c.Close())
		assert.Equal(t, []string{
			"cleanup res1",
			"cleanup res2",
			"cleanup shared",
		}, log.recorded())
	})
}

func TestCleanupErrors(t *testing.T) {
	t.Run("failures do not stop remaining cleanups", func(t *testing.T) {
		log := new(eventLog)
		c := New()

		type A struct{}
		type B struct{}
		type C struct{}

		require.NoError(t, c.Provide(func() *A { return &A{} },
			WithProviderCleanup(log.cleanup("A", errors.New("A failed")))))
		require.NoError(t, c.Provide(func() *B { return &B{} },
			WithProviderCleanup(log.cleanup("B", nil))))
		require.NoError(t, c.Provide(func() *C { return &C{} },
			WithProviderCleanup(log.cleanup("C", errors.New("C failed")))))

		require.NoError(t, c.Invoke(func(*A, *B, *C) {}))

		err := c.Close()
		require.Error(t, err)
		assert.ErrorContains(t, err, "A failed")
		assert.ErrorContains(t, err, "C failed")

		assert.Equal(t, []string{"cleanup C", "cleanup B", "cleanup A"}, log.recorded(),
			"a failing cleanup must not forget the remaining resources")
	})

	t.Run("errors are preserved and Close is idempotent", func(t *testing.T) {
		log := new(eventLog)
		c := New()

		type A struct{}

		require.NoError(t, c.Provide(func() *A { return &A{} },
			WithProviderCleanup(log.cleanup("A", errors.New("A failed")))))
		require.NoError(t, c.Invoke(func(*A) {}))

		err1 := c.Close()
		require.Error(t, err1)

		err2 := c.Close()
		assert.Equal(t, err1.Error(), err2.Error(),
			"repeated Close must report a consistent state")
		assert.Equal(t, []string{"cleanup A"}, log.recorded(),
			"repeated Close must not re-run callbacks")
	})

	t.Run("child cleanup failure does not stop parent cleanup", func(t *testing.T) {
		log := new(eventLog)
		c := New()

		type shared struct{}
		type childRes struct{}

		require.NoError(t, c.Provide(func() *shared { return &shared{} },
			WithProviderCleanup(log.cleanup("shared", nil))))

		child := c.Scope("child")
		require.NoError(t, child.Provide(func(*shared) *childRes { return &childRes{} },
			WithProviderCleanup(log.cleanup("childRes", errors.New("child failed")))))

		require.NoError(t, child.Invoke(func(*childRes) {}))

		err := c.Close()
		require.Error(t, err)
		assert.ErrorContains(t, err, "child failed")
		assert.Equal(t, []string{"cleanup childRes", "cleanup shared"}, log.recorded(),
			"parent resources must still be released when a child cleanup fails")
	})
}

func TestCloseForbidsInvoke(t *testing.T) {
	c := New()

	type A struct{}
	require.NoError(t, c.Provide(func() *A { return &A{} }))
	require.NoError(t, c.Invoke(func(*A) {}))
	require.NoError(t, c.Close())

	err := c.Invoke(func(*A) {})
	require.Error(t, err)
	assert.ErrorContains(t, err, "closed")

	child := c.Scope("child")
	err = child.Invoke(func(*A) {})
	require.Error(t, err)
	assert.ErrorContains(t, err, `scope "child" is closed`)
}

func TestCleanupCallbackInfo(t *testing.T) {
	t.Run("cleanup error names the constructor", func(t *testing.T) {
		c := New()

		type A struct{}
		require.NoError(t, c.Provide(func() *A { return &A{} },
			WithProviderCleanup(func() error { return errors.New("boom") })))
		require.NoError(t, c.Invoke(func(*A) {}))

		err := c.Close()
		require.Error(t, err)
		assert.Contains(t, fmt.Sprintf("%v", err), "cleanup")
		assert.Contains(t, fmt.Sprintf("%v", err), "boom")
	})
}
