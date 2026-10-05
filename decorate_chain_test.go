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
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.uber.org/dig"
	"go.uber.org/dig/internal/digtest"
)

// TestDecorationChainSimple is the motivating example: a request-tag stage
// followed by an audit stage for the same type in the same Scope. Each stage
// receives the output of its predecessor, and both construct at most once.
func TestDecorationChainSimple(t *testing.T) {
	t.Parallel()

	type Logger struct{ name string }

	var ctorCalls int32
	c := digtest.New(t)
	c.RequireProvide(func() *Logger {
		atomic.AddInt32(&ctorCalls, 1)
		return &Logger{name: "base"}
	})

	var tagCalls, auditCalls int32
	chain := c.DecorationChain("logger")
	require.Equal(t, "logger", chain.Name())
	require.NoError(t, chain.Decorate("request-tag", func(log *Logger) *Logger {
		atomic.AddInt32(&tagCalls, 1)
		return &Logger{name: log.name + "+tag"}
	}))
	require.NoError(t, chain.Decorate("audit", func(log *Logger) *Logger {
		atomic.AddInt32(&auditCalls, 1)
		return &Logger{name: log.name + "+audit"}
	}))

	for i := 0; i < 3; i++ {
		c.RequireInvoke(func(log *Logger) {
			assert.Equal(t, "base+tag+audit", log.name)
		})
	}

	assert.Equal(t, int32(1), atomic.LoadInt32(&ctorCalls))
	assert.Equal(t, int32(1), atomic.LoadInt32(&tagCalls))
	assert.Equal(t, int32(1), atomic.LoadInt32(&auditCalls))
}

// A stage that injects a type it does not produce resolves it through the
// normal dependency rules, while its own output comes from the previous
// stage.
func TestDecorationChainOtherDeps(t *testing.T) {
	t.Parallel()

	type Logger struct{ name string }
	type Config struct{ tag string }

	c := digtest.New(t)
	c.RequireProvide(func() *Logger { return &Logger{name: "base"} })
	c.RequireProvide(func() *Config { return &Config{tag: "req"} })

	c.RequireChainDecorate("logger", "tag", func(log *Logger, cfg *Config) *Logger {
		return &Logger{name: log.name + "+" + cfg.tag}
	})
	c.RequireChainDecorate("logger", "audit", func(log *Logger, cfg *Config) *Logger {
		return &Logger{name: log.name + "+audit:" + cfg.tag}
	})

	c.RequireInvoke(func(log *Logger) {
		assert.Equal(t, "base+req+audit:req", log.name)
	})
}

// A multi-output stage commits all of its outputs together, and a later
// multi-output stage can decorate both of them while observing both
// previous results.
func TestDecorationChainMultiOutput(t *testing.T) {
	t.Parallel()

	type A struct{ value string }
	type B struct{ value string }

	c := digtest.New(t)
	c.RequireProvide(func() *A { return &A{value: "a0"} })
	c.RequireProvide(func() *B { return &B{value: "b0"} })

	chain := c.DecorationChain("ab")
	require.NoError(t, chain.Decorate("enrich", func(a *A, b *B) (*A, *B) {
		return &A{value: a.value + "-e"}, &B{value: b.value + "-e"}
	}))
	require.NoError(t, chain.Decorate("combine", func(a *A, b *B) (*A, *B) {
		return &A{value: a.value + "+" + b.value}, &B{value: b.value + "+" + a.value}
	}))

	c.RequireInvoke(func(a *A, b *B) {
		assert.Equal(t, "a0-e+b0-e", a.value)
		assert.Equal(t, "b0-e+a0-e", b.value)
	})
}

// Two outputs of the same stage may participate in each other's decoration
// within that stage: they are committed together.
func TestDecorationChainSameStageMultiOutput(t *testing.T) {
	t.Parallel()

	type A struct{ tag string }
	type B struct{ tag string }

	c := digtest.New(t)
	c.RequireProvide(func() *A { return &A{tag: "a"} })
	c.RequireProvide(func() *B { return &B{tag: "b"} })

	// Two separate stages, each multi-output, declared in the same chain.
	c.RequireChainDecorate("ab", "both", func(a *A, b *B) (*A, *B) {
		return &A{tag: a.tag + "1"}, &B{tag: b.tag + "1"}
	})
	c.RequireChainDecorate("ab", "cross", func(a *A, b *B) (*A, *B) {
		return &A{tag: a.tag + "/" + b.tag}, &B{tag: b.tag + "/" + a.tag}
	})

	c.RequireInvoke(func(a *A, b *B) {
		assert.Equal(t, "a1/b1", a.tag)
		assert.Equal(t, "b1/a1", b.tag)
	})
}

// A failing stage commits nothing and can be retried later; earlier stages
// are not re-run. The failure must not cache results for later stages.
func TestDecorationChainFailureNotCached(t *testing.T) {
	t.Parallel()

	type A struct{ value int }

	var baseCalls, stage1Calls, stage2Calls int32
	c := digtest.New(t)
	c.RequireProvide(func() *A {
		atomic.AddInt32(&baseCalls, 1)
		return &A{value: 0}
	})
	c.RequireChainDecorate("a", "one", func(a *A) *A {
		atomic.AddInt32(&stage1Calls, 1)
		return &A{value: a.value + 1}
	})

	var shouldFail atomic.Bool
	shouldFail.Store(true)
	require.NoError(t, c.DecorationChain("a").Decorate("two", func(a *A) (*A, error) {
		atomic.AddInt32(&stage2Calls, 1)
		if shouldFail.Load() {
			return nil, errors.New("great sadness")
		}
		return &A{value: a.value + 10}, nil
	}))

	err := c.Invoke(func(a *A) {})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "great sadness")

	// Stage 1 succeeded and stays cached; stage 2 did not commit.
	assert.Equal(t, int32(1), atomic.LoadInt32(&baseCalls))
	assert.Equal(t, int32(1), atomic.LoadInt32(&stage1Calls))
	assert.Equal(t, int32(1), atomic.LoadInt32(&stage2Calls))

	// The container must still expose stage 1's cached output as a
	// decorated value while stage 2 remains broken; after the failure goes
	// away, stage 2 retries without re-running stage 1.
	shouldFail.Store(false)
	c.RequireInvoke(func(a *A) {
		assert.Equal(t, 11, a.value)
	})
	assert.Equal(t, int32(2), atomic.LoadInt32(&stage2Calls))
	assert.Equal(t, int32(1), atomic.LoadInt32(&stage1Calls))
}

// A multi-output stage that fails must not leave any of its outputs cached,
// and once it stops failing it commits all outputs together.
func TestDecorationChainMultiOutputFailure(t *testing.T) {
	t.Parallel()

	type A struct{ v string }
	type B struct{ v string }

	var stageCalls int32
	var shouldFail atomic.Bool
	shouldFail.Store(true)

	c := digtest.New(t)
	c.RequireProvide(func() *A { return &A{v: "a"} })
	c.RequireProvide(func() *B { return &B{v: "b"} })

	require.NoError(t, c.DecorationChain("ab").Decorate("split", func(a *A, b *B) (aout *A, bout *B, err error) {
		atomic.AddInt32(&stageCalls, 1)
		if shouldFail.Load() {
			return nil, nil, errors.New("boom")
		}
		return &A{v: a.v + "x"}, &B{v: b.v + "x"}, nil
	}))

	err := c.Invoke(func(a *A, b *B) {})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
	assert.Equal(t, int32(1), atomic.LoadInt32(&stageCalls))

	// While failing, every request retries the stage rather than caching the
	// partial or failed result.
	err = c.Invoke(func(a *A, b *B) {
		assert.Fail(t, "failing stage must not produce a value")
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
	assert.Equal(t, int32(2), atomic.LoadInt32(&stageCalls))

	// Once the failure clears, the whole stage retries and commits together.
	shouldFail.Store(false)
	c.RequireInvoke(func(a *A, b *B) {
		assert.Equal(t, "ax", a.v)
		assert.Equal(t, "bx", b.v)
	})
	assert.Equal(t, int32(3), atomic.LoadInt32(&stageCalls))

	// After success it constructs at most once and stays cached.
	c.RequireInvoke(func(a *A, b *B) {
		assert.Equal(t, "ax", a.v)
		assert.Equal(t, "bx", b.v)
	})
	assert.Equal(t, int32(3), atomic.LoadInt32(&stageCalls))
}

// Named values follow the same chain semantics: the name is part of the key,
// so each named value gets its own chain and distinct names never interfere.
func TestDecorationChainNamed(t *testing.T) {
	t.Parallel()

	type In struct {
		dig.In

		Primary   string `name:"primary"`
		Secondary string `name:"secondary"`
	}

	c := digtest.New(t)
	c.RequireProvide(func() string { return "p" }, dig.Name("primary"))
	c.RequireProvide(func() string { return "s" }, dig.Name("secondary"))

	type PrimaryIn struct {
		dig.In
		Value string `name:"primary"`
	}
	type PrimaryOut struct {
		dig.Out
		Value string `name:"primary"`
	}
	type SecondaryIn struct {
		dig.In
		Value string `name:"secondary"`
	}
	type SecondaryOut struct {
		dig.Out
		Value string `name:"secondary"`
	}

	c.RequireChainDecorate("primary", "one", func(in PrimaryIn) PrimaryOut {
		assert.Equal(t, "p", in.Value)
		return PrimaryOut{Value: in.Value + "-1"}
	})
	c.RequireChainDecorate("primary", "two", func(in PrimaryIn) PrimaryOut {
		assert.Equal(t, "p-1", in.Value)
		return PrimaryOut{Value: in.Value + "-2"}
	})
	c.RequireChainDecorate("secondary", "one", func(in SecondaryIn) SecondaryOut {
		assert.Equal(t, "s", in.Value)
		return SecondaryOut{Value: in.Value + "-1"}
	})

	c.RequireInvoke(func(in In) {
		assert.Equal(t, "p-1-2", in.Primary)
		assert.Equal(t, "s-1", in.Secondary)
	})
}

// Registration errors: duplicate stage names and mixing with legacy Decorate.
func TestDecorationChainRegistrationErrors(t *testing.T) {
	t.Parallel()

	type A struct{ v string }

	t.Run("empty stage name", func(t *testing.T) {
		t.Parallel()
		c := digtest.New(t)
		err := c.DecorationChain("c").Decorate("", func(a *A) *A { return a })
		require.Error(t, err)
		assert.Contains(t, err.Error(), "stage name must not be empty")
	})

	t.Run("duplicate stage name", func(t *testing.T) {
		t.Parallel()
		c := digtest.New(t)
		chain := c.DecorationChain("c")
		require.NoError(t, chain.Decorate("one", func(a *A) *A { return a }))
		err := chain.Decorate("one", func(a *A) *A { return a })
		require.Error(t, err)
		assert.Contains(t, err.Error(), `stage "one" is already registered`)
	})

	t.Run("legacy Decorate conflicts with chain", func(t *testing.T) {
		t.Parallel()
		c := digtest.New(t)
		c.RequireProvide(func() *A { return &A{} })
		c.RequireChainDecorate("c", "one", func(a *A) *A { return a })
		err := c.Decorate(func(a *A) *A { return a })
		require.Error(t, err)
		assert.Contains(t, err.Error(), "*dig_test.A already decorated")
	})

	t.Run("chain conflicts with legacy Decorate", func(t *testing.T) {
		t.Parallel()
		c := digtest.New(t)
		c.RequireProvide(func() *A { return &A{} })
		c.RequireDecorate(func(a *A) *A { return a })
		err := c.DecorationChain("c").Decorate("one", func(a *A) *A { return a })
		require.Error(t, err)
		assert.Contains(t, err.Error(), "*dig_test.A already decorated")
	})
}

// Two outputs that participate in decorating each other form a real cycle
// when their stages depend on each other:
//
//	chain "x" stage "needs-b": A <- B
//	chain "y" stage "needs-a": B <- A
//
// The error must identify both stages by their stable identities.
func TestDecorationChainCycleTwoOutputs(t *testing.T) {
	t.Parallel()

	type A struct{ v string }
	type B struct{ v string }

	cyclic := digtest.New(t)
	cyclic.RequireProvide(func() *A { return &A{v: "a"} })
	cyclic.RequireProvide(func() *B { return &B{v: "b"} })
	cyclic.RequireChainDecorate("x", "needs-b", func(a *A, b *B) *A {
		return &A{v: a.v + b.v}
	})
	cyclic.RequireChainDecorate("y", "needs-a", func(b *B, a *A) *B {
		return &B{v: b.v + a.v}
	})

	err := cyclic.Invoke(func(a *A, b *B) {})
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "cycle detected in decoration chain")
	assert.Contains(t, msg, `needs-b`)
	assert.Contains(t, msg, `needs-a`)
	assert.Contains(t, msg, "x")
	assert.Contains(t, msg, "y")
}

// Parent and child Scope chains must not share intermediate caches. The
// counters prove each layer's input comes from its designated predecessor,
// and both directions (parent-first, child-first) work.
func TestDecorationChainParentChildCaches(t *testing.T) {
	t.Parallel()

	type A struct{ value int }

	build := func(t *testing.T) (*digtest.Container, *digtest.Scope,
		*int32, *int32, *int32, *int32, *int32) {
		var baseCalls, p1Calls, p2Calls, c1Calls, c2Calls int32

		root := digtest.New(t)
		root.RequireProvide(func() *A {
			atomic.AddInt32(&baseCalls, 1)
			return &A{value: 0}
		})

		root.RequireChainDecorate("a", "parent-1", func(a *A) *A {
			atomic.AddInt32(&p1Calls, 1)
			assert.Equal(t, 0, a.value, "parent-1 input must be the base value")
			return &A{value: a.value + 1}
		})
		root.RequireChainDecorate("a", "parent-2", func(a *A) *A {
			atomic.AddInt32(&p2Calls, 1)
			assert.Equal(t, 1, a.value, "parent-2 input must be parent-1 output")
			return &A{value: a.value + 10}
		})

		child := root.Scope("child")
		child.RequireChainDecorate("a", "child-1", func(a *A) *A {
			atomic.AddInt32(&c1Calls, 1)
			assert.Equal(t, 11, a.value, "child-1 input must be parent-2 output")
			return &A{value: a.value + 100}
		})
		child.RequireChainDecorate("a", "child-2", func(a *A) *A {
			atomic.AddInt32(&c2Calls, 1)
			assert.Equal(t, 111, a.value, "child-2 input must be child-1 output")
			return &A{value: a.value + 1000}
		})

		return root, child, &baseCalls, &p1Calls, &p2Calls, &c1Calls, &c2Calls
	}

	t.Run("invoke child first", func(t *testing.T) {
		t.Parallel()
		root, child, base, p1, p2, c1, c2 := build(t)

		child.RequireInvoke(func(a *A) {
			assert.Equal(t, 1111, a.value)
		})
		root.RequireInvoke(func(a *A) {
			assert.Equal(t, 11, a.value)
		})
		// Second child invocation must reuse everything.
		child.RequireInvoke(func(a *A) {
			assert.Equal(t, 1111, a.value)
		})

		assert.Equal(t, int32(1), atomic.LoadInt32(base))
		assert.Equal(t, int32(1), atomic.LoadInt32(p1))
		assert.Equal(t, int32(1), atomic.LoadInt32(p2))
		assert.Equal(t, int32(1), atomic.LoadInt32(c1))
		assert.Equal(t, int32(1), atomic.LoadInt32(c2))
	})

	t.Run("invoke parent first", func(t *testing.T) {
		t.Parallel()
		root, child, base, p1, p2, c1, c2 := build(t)

		root.RequireInvoke(func(a *A) {
			assert.Equal(t, 11, a.value)
		})
		child.RequireInvoke(func(a *A) {
			assert.Equal(t, 1111, a.value)
		})

		assert.Equal(t, int32(1), atomic.LoadInt32(base))
		assert.Equal(t, int32(1), atomic.LoadInt32(p1))
		assert.Equal(t, int32(1), atomic.LoadInt32(p2))
		assert.Equal(t, int32(1), atomic.LoadInt32(c1))
		assert.Equal(t, int32(1), atomic.LoadInt32(c2))
	})
}

// Sibling child scopes must each run their own chain stages and must not see
// each other's intermediate (or final) decorated values.
func TestDecorationChainSiblingsDoNotShare(t *testing.T) {
	t.Parallel()

	type A struct{ value int }

	var left1, left2, right1, right2 int32

	root := digtest.New(t)
	root.RequireProvide(func() *A { return &A{value: 0} })

	left := root.Scope("left")
	left.RequireChainDecorate("a", "l1", func(a *A) *A {
		atomic.AddInt32(&left1, 1)
		return &A{value: a.value + 1}
	})
	left.RequireChainDecorate("a", "l2", func(a *A) *A {
		atomic.AddInt32(&left2, 1)
		assert.Equal(t, 1, a.value)
		return &A{value: a.value + 10}
	})

	right := root.Scope("right")
	right.RequireChainDecorate("a", "r1", func(a *A) *A {
		atomic.AddInt32(&right1, 1)
		assert.Equal(t, 0, a.value, "right chain must start from the base value, not left's output")
		return &A{value: a.value + 2}
	})
	right.RequireChainDecorate("a", "r2", func(a *A) *A {
		atomic.AddInt32(&right2, 1)
		assert.Equal(t, 2, a.value)
		return &A{value: a.value + 20}
	})

	left.RequireInvoke(func(a *A) { assert.Equal(t, 11, a.value) })
	right.RequireInvoke(func(a *A) { assert.Equal(t, 22, a.value) })

	assert.Equal(t, int32(1), atomic.LoadInt32(&left1))
	assert.Equal(t, int32(1), atomic.LoadInt32(&left2))
	assert.Equal(t, int32(1), atomic.LoadInt32(&right1))
	assert.Equal(t, int32(1), atomic.LoadInt32(&right2))
}

// Legacy single Decorate still works after chains were added, and chains
// compose with existing decorators registered in an ancestor scope.
func TestDecorationChainMixesWithLegacyAncestor(t *testing.T) {
	t.Parallel()

	type A struct{ value string }

	root := digtest.New(t)
	root.RequireProvide(func() *A { return &A{value: "a"} })
	root.RequireDecorate(func(a *A) *A { return &A{value: a.value + "-legacy"} })

	child := root.Scope("child")
	child.RequireChainDecorate("a", "only", func(a *A) *A {
		return &A{value: a.value + "-chain"}
	})

	root.RequireInvoke(func(a *A) { assert.Equal(t, "a-legacy", a.value) })
	child.RequireInvoke(func(a *A) {
		assert.Equal(t, "a-legacy-chain", a.value)
	})
}

// Value groups support chains too: each stage receives the group produced by
// the previous stage.
func TestDecorationChainValueGroups(t *testing.T) {
	t.Parallel()

	type In struct {
		dig.In
		Values []int `group:"vals"`
	}
	type Out struct {
		dig.Out
		Values []int `group:"vals"`
	}

	c := digtest.New(t)
	for _, v := range []int{1, 2, 3} {
		v := v
		c.RequireProvide(func() int { return v }, dig.Group("vals"))
	}

	c.RequireChainDecorate("vals", "increment", func(in In) Out {
		out := Out{Values: make([]int, len(in.Values))}
		for i, v := range in.Values {
			out.Values[i] = v + 1
		}
		return out
	})
	c.RequireChainDecorate("vals", "append", func(in In) Out {
		return Out{Values: append(append([]int{}, in.Values...), 100)}
	})

	c.RequireInvoke(func(in In) {
		assert.ElementsMatch(t, []int{2, 3, 4, 100}, in.Values)
	})
}
