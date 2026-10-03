// Copyright (c) 2021 Uber Technologies, Inc.
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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type bindIfaceA interface{ A() int }
type bindIfaceB interface{ B() int }
type bindIfaceC interface{ C() int }

// bindBoth implements both bindIfaceA and bindIfaceB with a single output.
type bindBoth struct{ id int }

func (b bindBoth) A() int { return b.id }
func (b bindBoth) B() int { return b.id }

// bindPtrOnly implements bindIfaceA only on its pointer receiver.
type bindPtrOnly struct{ id int }

func (p *bindPtrOnly) A() int { return p.id }

// bindSelf implements bindIfaceA but its constructor consumes bindIfaceA.
type bindSelf struct{}

func (bindSelf) A() int { return 0 }

type bindAlt1 struct{}

func (bindAlt1) A() int { return 1 }

type bindAlt2 struct{}

func (bindAlt2) A() int { return 2 }

type bindNamed struct{}

func (bindNamed) A() int { return 0 }

type bindGrouped struct{}

func (bindGrouped) A() int { return 0 }

type bindAsImpl struct{}

func (*bindAsImpl) A() int { return 7 }

type bindScoped struct{ id int }

func (b bindScoped) A() int { return b.id }

func TestPlanInterfaceBindingsOneOutputTwoInterfaces(t *testing.T) {
	c := New()

	called := false
	require.NoError(t, c.Provide(func() bindBoth {
		called = true
		return bindBoth{id: 42}
	}))

	plan, err := c.PlanInterfaceBindings(new(bindIfaceA), new(bindIfaceB))
	require.NoError(t, err)
	require.NoError(t, plan.Commit())

	// Planning and committing inspect signatures only.
	assert.False(t, called, "constructor must not run during plan/commit")

	require.NoError(t, c.Invoke(func(a bindIfaceA, b bindIfaceB) {
		assert.Equal(t, 42, a.A())
		assert.Equal(t, 42, b.B())
	}))
	assert.True(t, called)
}

func TestPlanInterfaceBindingsMethodSets(t *testing.T) {
	t.Run("value type does not implement pointer interface", func(t *testing.T) {
		c := New()
		require.NoError(t, c.Provide(func() bindPtrOnly { return bindPtrOnly{id: 1} }))

		_, err := c.PlanInterfaceBindings(new(bindIfaceA))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no candidate for interface dig.bindIfaceA")
	})

	t.Run("pointer type implements", func(t *testing.T) {
		c := New()
		require.NoError(t, c.Provide(func() *bindPtrOnly { return &bindPtrOnly{id: 2} }))

		plan, err := c.PlanInterfaceBindings(new(bindIfaceA))
		require.NoError(t, err)
		require.NoError(t, plan.Commit())

		require.NoError(t, c.Invoke(func(a bindIfaceA) {
			assert.Equal(t, 2, a.A())
		}))
	})
}

func TestPlanInterfaceBindingsSelfDependentCandidate(t *testing.T) {
	c := New()
	require.NoError(t, c.Provide(func(a bindIfaceA) bindSelf { return bindSelf{} }))

	// Planning only checks signatures, so it succeeds.
	plan, err := c.PlanInterfaceBindings(new(bindIfaceA))
	require.NoError(t, err)

	// Installing the binding would make the candidate's constructor
	// depend on itself.
	err = plan.Commit()
	require.Error(t, err)
	assert.True(t, IsCycleDetected(err), "expected a cycle error, got: %v", err)
	assert.Contains(t, err.Error(), "dig.bindSelf")

	// The failed commit must not leave any binding behind: the next
	// Invoke fails exactly as it would have before the plan.
	err = c.Invoke(func(bindIfaceA) {})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing type")
}

func TestPlanInterfaceBindingsFailedPlanLeavesNoBindings(t *testing.T) {
	c := New()
	require.NoError(t, c.Provide(func() bindBoth { return bindBoth{id: 1} }))

	invoke := func() error { return c.Invoke(func(bindIfaceA) {}) }
	before := invoke()
	require.Error(t, before)

	// bindIfaceA has a unique candidate but bindIfaceC has none, so the
	// whole plan fails and no binding may be installed.
	_, err := c.PlanInterfaceBindings(new(bindIfaceA), new(bindIfaceC))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no candidate for interface dig.bindIfaceC")

	after := invoke()
	require.Error(t, after)
	assert.Equal(t, before.Error(), after.Error(),
		"failed batch plan must not change the result of the next Invoke")
}

func TestPlanInterfaceBindingsNamedAndGroupedExcluded(t *testing.T) {
	t.Run("named value is not a candidate", func(t *testing.T) {
		c := New()
		require.NoError(t, c.Provide(func() bindNamed { return bindNamed{} }, Name("n")))

		_, err := c.PlanInterfaceBindings(new(bindIfaceA))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no candidate")
	})

	t.Run("grouped value is not a candidate", func(t *testing.T) {
		type out struct {
			Out

			V bindGrouped `group:"g"`
		}
		c := New()
		require.NoError(t, c.Provide(func() out { return out{V: bindGrouped{}} }))

		_, err := c.PlanInterfaceBindings(new(bindIfaceA))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no candidate")
	})
}

func TestPlanInterfaceBindingsExistingProviderWins(t *testing.T) {
	c := New()
	require.NoError(t, c.Provide(func() *bindAsImpl { return &bindAsImpl{} }, As(new(bindIfaceA))))

	// The interface already has an explicit provider, so the plan binds
	// nothing and commits as a no-op.
	plan, err := c.PlanInterfaceBindings(new(bindIfaceA))
	require.NoError(t, err)
	require.NoError(t, plan.Commit())

	require.NoError(t, c.Invoke(func(a bindIfaceA) {
		assert.Equal(t, 7, a.A())
	}))
}

func TestPlanInterfaceBindingsAmbiguous(t *testing.T) {
	c := New()
	require.NoError(t, c.Provide(func() bindAlt1 { return bindAlt1{} }))
	require.NoError(t, c.Provide(func() bindAlt2 { return bindAlt2{} }))

	_, err := c.PlanInterfaceBindings(new(bindIfaceA))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multiple candidates for interface dig.bindIfaceA")
	assert.Contains(t, err.Error(), "dig.bindAlt1")
	assert.Contains(t, err.Error(), "dig.bindAlt2")

	// The caller can still disambiguate manually by registering an
	// explicit interface provider; a new plan then leaves it alone.
	require.NoError(t, c.Provide(func(a bindAlt2) bindIfaceA { return a }))
	plan, err := c.PlanInterfaceBindings(new(bindIfaceA))
	require.NoError(t, err)
	require.NoError(t, plan.Commit())

	require.NoError(t, c.Invoke(func(a bindIfaceA) {
		assert.Equal(t, 2, a.A())
	}))
}

func TestPlanInterfaceBindingsStalePlanRejected(t *testing.T) {
	t.Run("registration on same scope", func(t *testing.T) {
		c := New()
		require.NoError(t, c.Provide(func() bindBoth { return bindBoth{id: 1} }))

		plan, err := c.PlanInterfaceBindings(new(bindIfaceA))
		require.NoError(t, err)

		require.NoError(t, c.Provide(func() string { return "" }))

		err = plan.Commit()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "stale")

		// The rejected commit installed nothing.
		err = c.Invoke(func(bindIfaceA) {})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "missing type")
	})

	t.Run("registration on ancestor scope", func(t *testing.T) {
		c := New()
		child := c.Scope("child")
		require.NoError(t, child.Provide(func() bindBoth { return bindBoth{id: 1} }))

		plan, err := child.PlanInterfaceBindings(new(bindIfaceA))
		require.NoError(t, err)

		require.NoError(t, c.Provide(func() string { return "" }))

		err = plan.Commit()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "stale")
	})

	t.Run("committing twice", func(t *testing.T) {
		c := New()
		require.NoError(t, c.Provide(func() bindBoth { return bindBoth{id: 1} }))

		plan, err := c.PlanInterfaceBindings(new(bindIfaceA))
		require.NoError(t, err)
		require.NoError(t, plan.Commit())
		require.Error(t, plan.Commit())
	})
}

func TestPlanInterfaceBindingsScopeVisibility(t *testing.T) {
	t.Run("child providers are not visible to parent", func(t *testing.T) {
		c := New()
		child := c.Scope("child")
		require.NoError(t, child.Provide(func() bindScoped { return bindScoped{id: 1} }))

		_, err := c.PlanInterfaceBindings(new(bindIfaceA))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no candidate")

		plan, err := child.PlanInterfaceBindings(new(bindIfaceA))
		require.NoError(t, err)
		require.NoError(t, plan.Commit())
		require.NoError(t, child.Invoke(func(a bindIfaceA) {
			assert.Equal(t, 1, a.A())
		}))
	})

	t.Run("closer scope shadows ancestors", func(t *testing.T) {
		c := New()
		require.NoError(t, c.Provide(func() bindScoped { return bindScoped{id: 1} }))

		child := c.Scope("child")
		require.NoError(t, child.Provide(func() bindScoped { return bindScoped{id: 2} }))

		// The child scope's provider shadows the parent's, so it is the
		// unique candidate rather than an ambiguity.
		plan, err := child.PlanInterfaceBindings(new(bindIfaceA))
		require.NoError(t, err)
		require.NoError(t, plan.Commit())
		require.NoError(t, child.Invoke(func(a bindIfaceA) {
			assert.Equal(t, 2, a.A())
		}))
	})
}

func TestPlanInterfaceBindingsInvalidInput(t *testing.T) {
	c := New()

	_, err := c.PlanInterfaceBindings()
	require.Error(t, err)

	_, err = c.PlanInterfaceBindings(nil)
	require.Error(t, err)

	_, err = c.PlanInterfaceBindings(new(int))
	require.Error(t, err)

	_, err = c.PlanInterfaceBindings(bindIfaceA(nil))
	require.Error(t, err)

	_, err = c.PlanInterfaceBindings(new(bindIfaceA), new(bindIfaceA))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "more than once")
}
