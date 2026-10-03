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
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/dig"
)

// pointerOnlyReader implements io.Reader on its pointer type only.
type pointerOnlyReader struct{}

func (*pointerOnlyReader) Read([]byte) (int, error) { return 0, io.EOF }

// selfReader implements io.Reader but its constructor depends on io.Reader,
// so binding io.Reader to it introduces a dependency cycle.
type selfReader struct{ r io.Reader }

func (s *selfReader) Read(p []byte) (int, error) { return s.r.Read(p) }

// onlyWriter implements io.Writer and nothing else.
type onlyWriter struct{ buf *bytes.Buffer }

func (w *onlyWriter) Write(p []byte) (int, error) { return w.buf.Write(p) }

// frobnicator is an interface nothing in these tests implements.
type frobnicator interface{ Frobnicate() }

func TestInterfaceBindingPlanOneOutputTwoInterfaces(t *testing.T) {
	c := dig.New()

	var constructed int
	require.NoError(t, c.Provide(func() *bytes.Buffer {
		constructed++
		return bytes.NewBufferString("hello")
	}))

	// A single output implements both interfaces.
	plan, err := c.PlanInterfaceBindings(new(io.Reader), new(io.Writer))
	require.NoError(t, err)
	assert.Equal(t, 0, constructed, "planning must not execute constructors")
	assert.Contains(t, plan.String(), "io.Reader")
	assert.Contains(t, plan.String(), "io.Writer")

	require.NoError(t, plan.Commit())
	assert.Equal(t, 0, constructed, "committing must not execute constructors")

	require.NoError(t, c.Invoke(func(r io.Reader, w io.Writer) {
		_, err := w.Write([]byte("!"))
		require.NoError(t, err)
		got, err := io.ReadAll(r)
		require.NoError(t, err)
		assert.Equal(t, "hello!", string(got),
			"both interfaces should be backed by the same instance")
	}))
	assert.Equal(t, 1, constructed)
}

func TestInterfaceBindingPlanPointerMethodSet(t *testing.T) {
	t.Run("value type does not gain pointer methods", func(t *testing.T) {
		c := dig.New()
		require.NoError(t, c.Provide(func() pointerOnlyReader { return pointerOnlyReader{} }))

		_, err := c.PlanInterfaceBindings(new(io.Reader))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no unnamed, ungrouped provider output")
	})

	t.Run("pointer type satisfies the interface", func(t *testing.T) {
		c := dig.New()
		require.NoError(t, c.Provide(func() *pointerOnlyReader { return &pointerOnlyReader{} }))

		plan, err := c.PlanInterfaceBindings(new(io.Reader))
		require.NoError(t, err)
		require.NoError(t, plan.Commit())
		require.NoError(t, c.Invoke(func(io.Reader) {}))
	})
}

func TestInterfaceBindingPlanSelfDependencyCycle(t *testing.T) {
	c := dig.New()
	require.NoError(t, c.Provide(func(r io.Reader) *selfReader { return &selfReader{r: r} }))

	plan, err := c.PlanInterfaceBindings(new(io.Reader))
	require.NoError(t, err, "the self-referential candidate is unique")

	err = plan.Commit()
	require.Error(t, err)
	assert.True(t, dig.IsCycleDetected(err), "expected a cycle error, got %v", err)
	assert.Contains(t, err.Error(), "*dig_test.selfReader")
	assert.Contains(t, err.Error(), "[0]")

	// The failed commit must not leave any binding behind.
	err = c.Invoke(func(io.Reader) {})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing type")
}

func TestInterfaceBindingPlanMultipleCandidates(t *testing.T) {
	c := dig.New()
	require.NoError(t, c.Provide(func() *bytes.Buffer { return bytes.NewBufferString("buf") }))
	require.NoError(t, c.Provide(func() *strings.Reader { return strings.NewReader("str") }))

	_, err := c.PlanInterfaceBindings(new(io.Reader))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multiple provider outputs")
	assert.Contains(t, err.Error(), "*bytes.Buffer")
	assert.Contains(t, err.Error(), "*strings.Reader")
	assert.Contains(t, err.Error(), "[0]")

	// The caller can still disambiguate manually; the explicit provider
	// takes precedence over any planned binding.
	require.NoError(t, c.Provide(func(b *bytes.Buffer) io.Reader { return b }))
	plan, err := c.PlanInterfaceBindings(new(io.Reader))
	require.NoError(t, err)
	require.NoError(t, plan.Commit())

	require.NoError(t, c.Invoke(func(r io.Reader) {
		got, err := io.ReadAll(r)
		require.NoError(t, err)
		assert.Equal(t, "buf", string(got))
	}))
}

func TestInterfaceBindingPlanExplicitAsTakesPrecedence(t *testing.T) {
	c := dig.New()
	require.NoError(t, c.Provide(
		func() *bytes.Buffer { return bytes.NewBufferString("as") },
		dig.As(new(io.Reader)),
	))
	require.NoError(t, c.Provide(func() *strings.Reader { return strings.NewReader("str") }))

	plan, err := c.PlanInterfaceBindings(new(io.Reader))
	require.NoError(t, err)
	require.NoError(t, plan.Commit())

	require.NoError(t, c.Invoke(func(r io.Reader) {
		got, err := io.ReadAll(r)
		require.NoError(t, err)
		assert.Equal(t, "as", string(got))
	}))
}

func TestInterfaceBindingPlanIgnoresNamedAndGrouped(t *testing.T) {
	c := dig.New()
	require.NoError(t, c.Provide(func() *bytes.Buffer { return nil }, dig.Name("named")))
	require.NoError(t, c.Provide(func() *strings.Reader { return nil }, dig.Group("readers")))

	_, err := c.PlanInterfaceBindings(new(io.Reader))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no unnamed, ungrouped provider output")
}

func TestInterfaceBindingPlanScopeVisibility(t *testing.T) {
	t.Run("child outputs are not visible from the root", func(t *testing.T) {
		c := dig.New()
		child := c.Scope("child")
		require.NoError(t, child.Provide(func() *bytes.Buffer { return nil }))

		_, err := c.PlanInterfaceBindings(new(io.Reader))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no unnamed, ungrouped provider output")
	})

	t.Run("nearer scope shadows ancestors", func(t *testing.T) {
		c := dig.New()
		require.NoError(t, c.Provide(func() *bytes.Buffer { return bytes.NewBufferString("root") }))
		child := c.Scope("child")
		require.NoError(t, child.Provide(func() *bytes.Buffer { return bytes.NewBufferString("child") }))

		plan, err := child.PlanInterfaceBindings(new(io.Reader))
		require.NoError(t, err)
		require.NoError(t, plan.Commit())

		require.NoError(t, child.Invoke(func(r io.Reader) {
			got, err := io.ReadAll(r)
			require.NoError(t, err)
			assert.Equal(t, "child", string(got))
		}))
	})
}

func TestInterfaceBindingPlanStale(t *testing.T) {
	t.Run("provide invalidates the plan", func(t *testing.T) {
		c := dig.New()
		require.NoError(t, c.Provide(func() *bytes.Buffer { return nil }))

		plan, err := c.PlanInterfaceBindings(new(io.Reader))
		require.NoError(t, err)

		require.NoError(t, c.Provide(func() int { return 42 }))
		err = plan.Commit()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no longer valid")

		// A fresh plan can still be created and committed.
		plan, err = c.PlanInterfaceBindings(new(io.Reader))
		require.NoError(t, err)
		require.NoError(t, plan.Commit())
		require.NoError(t, c.Invoke(func(io.Reader) {}))
	})

	t.Run("decorate invalidates the plan", func(t *testing.T) {
		c := dig.New()
		require.NoError(t, c.Provide(func() *bytes.Buffer { return nil }))

		plan, err := c.PlanInterfaceBindings(new(io.Reader))
		require.NoError(t, err)

		require.NoError(t, c.Decorate(func(b *bytes.Buffer) *bytes.Buffer { return b }))
		err = plan.Commit()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no longer valid")
	})
}

func TestInterfaceBindingPlanFailedBatchLeavesNothing(t *testing.T) {
	t.Run("planning failure", func(t *testing.T) {
		c := dig.New()
		require.NoError(t, c.Provide(func() *bytes.Buffer { return bytes.NewBufferString("buf") }))

		useReader := func(io.Reader) {}
		invokeBefore := c.Invoke(useReader)
		require.Error(t, invokeBefore)

		// io.Reader has a unique candidate but frobnicator has none: the
		// whole plan fails and no binding for io.Reader may be installed.
		_, err := c.PlanInterfaceBindings(new(io.Reader), new(frobnicator))
		require.Error(t, err)

		invokeAfter := c.Invoke(useReader)
		require.Error(t, invokeAfter)
		assert.Equal(t, invokeBefore.Error(), invokeAfter.Error(),
			"a failed plan must not change the result of the next Invoke")
	})

	t.Run("commit failure", func(t *testing.T) {
		c := dig.New()
		require.NoError(t, c.Provide(func() *onlyWriter { return &onlyWriter{buf: &bytes.Buffer{}} }))
		require.NoError(t, c.Provide(func(r io.Reader) *selfReader { return &selfReader{r: r} }))

		useWriter := func(io.Writer) {}
		invokeBefore := c.Invoke(useWriter)
		require.Error(t, invokeBefore)

		// io.Writer binds cleanly, but io.Reader introduces a cycle: the
		// whole commit fails and the io.Writer binding must not remain.
		plan, err := c.PlanInterfaceBindings(new(io.Writer), new(io.Reader))
		require.NoError(t, err)
		require.Error(t, plan.Commit())

		invokeAfter := c.Invoke(useWriter)
		require.Error(t, invokeAfter)
		assert.Equal(t, invokeBefore.Error(), invokeAfter.Error(),
			"a failed commit must not change the result of the next Invoke")
	})
}

func TestInterfaceBindingPlanResultObjectPosition(t *testing.T) {
	type out struct {
		dig.Out

		Reader *strings.Reader
	}

	c := dig.New()
	require.NoError(t, c.Provide(func() out { return out{Reader: strings.NewReader("x")} }))
	require.NoError(t, c.Provide(func() *bytes.Buffer { return nil }))

	_, err := c.PlanInterfaceBindings(new(io.Reader))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "*strings.Reader")
	assert.Contains(t, err.Error(), "[0].Reader")
}

func TestInterfaceBindingPlanInvalidTargets(t *testing.T) {
	c := dig.New()

	_, err := c.PlanInterfaceBindings(new(int))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pointer to an interface")

	_, err = c.PlanInterfaceBindings(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pointer to an interface")

	_, err = c.PlanInterfaceBindings("not a pointer")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pointer to an interface")
}

func TestInterfaceBindingPlanDoubleCommit(t *testing.T) {
	c := dig.New()
	require.NoError(t, c.Provide(func() *bytes.Buffer { return nil }))

	plan, err := c.PlanInterfaceBindings(new(io.Reader))
	require.NoError(t, err)
	require.NoError(t, plan.Commit())
	require.Error(t, plan.Commit())
}
