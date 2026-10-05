package dig_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.uber.org/dig"
	"go.uber.org/dig/internal/digtest"
)

func TestDecorateChain(t *testing.T) {
	t.Run("stages run in declaration order and receive the previous stage's output", func(t *testing.T) {
		t.Parallel()

		c := digtest.New(t)
		c.RequireProvide(func() int { return 1 })

		var inputs []int
		c.RequireDecorateChain(func(i int) int {
			inputs = append(inputs, i)
			return i + 1
		})
		c.RequireDecorateChain(func(i int) int {
			inputs = append(inputs, i)
			return i * 10
		})
		c.RequireDecorateChain(func(i int) int {
			inputs = append(inputs, i)
			return i + 2
		})

		c.RequireInvoke(func(i int) {
			assert.Equal(t, 22, i, "expected ((1 + 1) * 10) + 2")
		})
		assert.Equal(t, []int{1, 2, 20}, inputs,
			"each stage must be fed by its designated predecessor")
	})

	t.Run("a stage injecting other types resolves them normally", func(t *testing.T) {
		t.Parallel()

		c := digtest.New(t)
		c.RequireProvide(func() int { return 1 })
		c.RequireProvide(func() string { return "s" })

		var got string
		c.RequireDecorateChain(func(i int) int { return i + 1 })
		c.RequireDecorateChain(func(i int, s string) int {
			got = s
			return i * 2
		})

		c.RequireInvoke(func(i int) { assert.Equal(t, 4, i) })
		assert.Equal(t, "s", got)
	})

	t.Run("a multi-output stage commits all its outputs together", func(t *testing.T) {
		t.Parallel()

		type A struct{ V int }
		type B struct{ V int }

		c := digtest.New(t)
		c.RequireProvide(func() A { return A{1} })
		c.RequireProvide(func() B { return B{2} })

		calls := 0
		c.RequireDecorateChain(func(a A, b B) (A, B) {
			calls++
			return A{a.V * 10}, B{b.V * 10}
		})
		c.RequireDecorateChain(func(a A) A { return A{a.V + 1} })

		c.RequireInvoke(func(a A, b B) {
			assert.Equal(t, 11, a.V)
			assert.Equal(t, 20, b.V)
		})
		assert.Equal(t, 1, calls,
			"a multi-output stage must run exactly once for all its outputs")
	})

	t.Run("chains of named values are independent", func(t *testing.T) {
		t.Parallel()

		type namedIn struct {
			dig.In
			S string `name:"n"`
		}
		type namedOut struct {
			dig.Out
			S string `name:"n"`
		}

		c := digtest.New(t)
		c.RequireProvide(func() string { return "unnamed" })
		c.RequireProvide(func() string { return "named" }, dig.Name("n"))

		c.RequireDecorateChain(func(i namedIn) namedOut {
			return namedOut{S: i.S + "-stage1"}
		})
		c.RequireDecorateChain(func(i namedIn) namedOut {
			return namedOut{S: i.S + "-stage2"}
		})

		c.RequireInvoke(func(i namedIn) {
			assert.Equal(t, "named-stage1-stage2", i.S)
		})
		c.RequireInvoke(func(s string) {
			assert.Equal(t, "unnamed", s, "the unnamed value must not be decorated")
		})
	})

	t.Run("parent and child chains compose without sharing intermediate caches", func(t *testing.T) {
		t.Parallel()

		c := digtest.New(t)
		c.RequireProvide(func() int { return 1 })

		var rootInputs []int
		c.RequireDecorateChain(func(i int) int {
			rootInputs = append(rootInputs, i)
			return i + 10
		})
		c.RequireDecorateChain(func(i int) int {
			rootInputs = append(rootInputs, i)
			return i * 2
		})

		child := c.Scope("child")
		var childInputs []int
		child.RequireDecorateChain(func(i int) int {
			childInputs = append(childInputs, i)
			return i + 100
		})

		sibling := c.Scope("sibling")
		var siblingInputs []int
		sibling.RequireDecorateChain(func(i int) int {
			siblingInputs = append(siblingInputs, i)
			return i * 1000
		})

		child.RequireInvoke(func(i int) {
			assert.Equal(t, 122, i, "expected ((1 + 10) * 2) + 100")
		})
		c.RequireInvoke(func(i int) {
			assert.Equal(t, 22, i, "the root chain must not see the child scope's stages")
		})
		sibling.RequireInvoke(func(i int) {
			assert.Equal(t, 22000, i, "expected the sibling chain to build on the root chain's output")
		})

		assert.Equal(t, []int{1, 11}, rootInputs,
			"the root chain must run exactly once, each stage fed by its predecessor")
		assert.Equal(t, []int{22}, childInputs,
			"the child stage's input must be the root chain's final output")
		assert.Equal(t, []int{22}, siblingInputs,
			"the sibling stage's input must be the root chain's final output, not the child's")
	})

	t.Run("a scope without a chain inherits the parent's decorated value", func(t *testing.T) {
		t.Parallel()

		c := digtest.New(t)
		c.RequireProvide(func() int { return 1 })
		c.RequireDecorateChain(func(i int) int { return i + 1 })
		c.RequireDecorateChain(func(i int) int { return i * 10 })

		child := c.Scope("child")
		child.RequireInvoke(func(i int) { assert.Equal(t, 20, i) })
	})

	t.Run("a failed stage is retried and later stages are not cached", func(t *testing.T) {
		t.Parallel()

		c := digtest.New(t)
		c.RequireProvide(func() int { return 0 })

		var calls []string
		fail := true
		c.RequireDecorateChain(func(i int) int {
			calls = append(calls, "stage1")
			return i + 1
		})
		require.NoError(t, c.DecorateChain(func(i int) (int, error) {
			calls = append(calls, "stage2")
			if fail {
				return 0, errors.New("great sadness")
			}
			return i + 1, nil
		}))
		c.RequireDecorateChain(func(i int) int {
			calls = append(calls, "stage3")
			return i + 1
		})

		err := c.Invoke(func(int) {})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "great sadness")
		assert.Equal(t, []string{"stage1", "stage2"}, calls,
			"stage 3 must not run after stage 2 fails")

		fail = false
		c.RequireInvoke(func(i int) { assert.Equal(t, 3, i) })
		assert.Equal(t, []string{"stage1", "stage2", "stage2", "stage3"}, calls,
			"stage 1 ran at most once, stage 2 was retried, stage 3 ran after it")
	})

	t.Run("Decorate composes with DecorateChain but still rejects duplicates", func(t *testing.T) {
		t.Parallel()

		c := digtest.New(t)
		c.RequireProvide(func() int { return 1 })

		c.RequireDecorate(func(i int) int { return i + 1 })
		c.RequireDecorateChain(func(i int) int { return i * 10 })
		c.RequireInvoke(func(i int) { assert.Equal(t, 20, i) })

		err := c.Decorate(func(i int) int { return i })
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already decorated")
	})

	t.Run("value groups can be decorated in stages", func(t *testing.T) {
		t.Parallel()

		type groupIn struct {
			dig.In
			Values []string `group:"g"`
		}
		type groupOut struct {
			dig.Out
			Values []string `group:"g"`
		}

		c := digtest.New(t)
		c.RequireProvide(func() string { return "a" }, dig.Group("g"))
		c.RequireProvide(func() string { return "b" }, dig.Group("g"))

		var stageInputs [][]string
		c.RequireDecorateChain(func(i groupIn) groupOut {
			stageInputs = append(stageInputs, i.Values)
			return groupOut{Values: append(i.Values, "c")}
		})
		c.RequireDecorateChain(func(i groupIn) groupOut {
			stageInputs = append(stageInputs, i.Values)
			return groupOut{Values: append(i.Values, "d")}
		})

		c.RequireInvoke(func(i groupIn) {
			assert.ElementsMatch(t, []string{"a", "b", "c", "d"}, i.Values)
		})
		require.Len(t, stageInputs, 2)
		assert.NotContains(t, stageInputs[0], "c")
		assert.Contains(t, stageInputs[1], "c",
			"the second group stage must receive the first stage's output")
	})

	t.Run("FillDecorateInfo works for chain stages", func(t *testing.T) {
		t.Parallel()

		c := digtest.New(t)
		c.RequireProvide(func() int { return 1 })

		var info1, info2 dig.DecorateInfo
		c.RequireDecorateChain(func(i int) int { return i + 1 }, dig.FillDecorateInfo(&info1))
		c.RequireDecorateChain(func(i int) int { return i * 2 }, dig.FillDecorateInfo(&info2))

		assert.NotEqual(t, info1.ID, info2.ID)
		require.Len(t, info1.Inputs, 1)
		require.Len(t, info1.Outputs, 1)
		assert.Equal(t, "int", info1.Inputs[0].String())
		assert.Equal(t, "int", info2.Outputs[0].String())

		c.RequireInvoke(func(i int) { assert.Equal(t, 4, i) })
	})
}

func TestDecorateChainCycle(t *testing.T) {
	t.Run("two outputs mutually participating in decoration", func(t *testing.T) {
		t.Parallel()

		type A struct{}
		type B struct{}

		c := digtest.New(t)
		c.RequireProvide(func() B { return B{} })

		// A's chain stage 1 consumes B. A's chain stage 2 also produces B,
		// so B's only chain stage needs A while A's first stage is running.
		c.RequireDecorateChain(func(b B) A { return A{} })
		c.RequireDecorateChain(func(a A) (A, B) { return A{}, B{} })

		err := c.Invoke(func(A) {})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cycle detected")
		assert.Contains(t, err.Error(), "stage 1 of its decorate chain")
		assert.Contains(t, err.Error(), "dig_test.A")
	})

	t.Run("another type depending back on the running stage", func(t *testing.T) {
		t.Parallel()

		type A struct{}
		type B struct{}

		c := digtest.New(t)
		c.RequireProvide(func(a A) B { return B{} })
		c.RequireDecorateChain(func(b B) A { return A{} })

		err := c.Invoke(func(A) {})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cycle detected")
		assert.Contains(t, err.Error(), "stage 1 of its decorate chain")
		assert.Contains(t, err.Error(), "dig_test.A")
	})
}
