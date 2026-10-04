// Copyright (c) 2026 Uber Technologies, Inc.
//
// Licensed under the same terms as the rest of the dig project.

package dig

import (
	"math/rand"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mw string

// taggedMW is a constructor returning a plain mw into group "mw" with the
// provided member tag, via the positional Group/GroupMemberTag options.
type groupProvider interface {
	Provide(constructor interface{}, opts ...ProvideOption) error
}

func provideMW(c groupProvider, name, tag string) {
	err := c.Provide(func() mw { return mw(name) }, Group("mw"), GroupMemberTag(tag))
	if err != nil {
		panic(err)
	}
}

type mwInOrdered struct {
	In
	Middlewares []mw `group:"mw,ordered"`
}

type mwInConstraints struct {
	In
	Middlewares []mw `group:"mw,ordered" groupbefore:"auth:audit,recover:*"`
}

func names(ms []mw) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, string(m))
	}
	return out
}

func TestOrderedGroup_BasicConstraintsWithShuffledRegistration(t *testing.T) {
	c := New()

	// Register in an order that deliberately contradicts the desired output.
	provideMW(c, "audit", "audit")
	provideMW(c, "recover", "recover")
	provideMW(c, "auth", "auth")
	provideMW(c, "metrics", "") // untagged

	type in struct {
		In
		Middlewares []mw `group:"mw,ordered" groupbefore:"auth:audit,recover:*"`
	}

	require.NoError(t, c.Invoke(func(i in) {
		got := names(i.Middlewares)
		// recover must be outside everything; auth must precede audit.
		require.Equal(t, "recover", got[0])
		assert.Less(t, indexOf(got, "auth"), indexOf(got, "audit"))
		assert.ElementsMatch(t, []string{"recover", "auth", "audit", "metrics"}, got)
	}))
}

func TestOrderedGroup_UnconstrainedStableByRegistration(t *testing.T) {
	c := New()
	provideMW(c, "c", "c")
	provideMW(c, "a", "a")
	provideMW(c, "b", "b")

	require.NoError(t, c.Invoke(func(i mwInOrdered) {
		// No constraints: registration order is preserved.
		assert.Equal(t, []string{"c", "a", "b"}, names(i.Middlewares))
	}))
}

func TestOrderedGroup_ConstraintOnlyAffectsRelatedMembers(t *testing.T) {
	c := New()
	provideMW(c, "x", "x")
	provideMW(c, "a", "a")
	provideMW(c, "b", "b")

	type in struct {
		In
		Middlewares []mw `group:"mw,ordered" groupbefore:"b:a"`
	}

	require.NoError(t, c.Invoke(func(i in) {
		got := names(i.Middlewares)
		// b must be before a; unrelated x keeps its earliest registration slot.
		assert.Equal(t, []string{"x", "b", "a"}, got)
	}))
}

func TestOrderedGroup_DiamondConstraint(t *testing.T) {
	c := New()
	provideMW(c, "d", "d")
	provideMW(c, "a", "a")
	provideMW(c, "b", "b")
	provideMW(c, "c", "c")

	type in struct {
		In
		// a and b are independent middle layers that both precede c, and d
		// precedes both: a diamond d -> {a,b} -> c.
		Middlewares []mw `group:"mw,ordered" groupbefore:"d:a,d:b,a:c,b:c"`
	}

	require.NoError(t, c.Invoke(func(i in) {
		got := names(i.Middlewares)
		assert.Equal(t, "d", got[0])
		assert.Equal(t, "c", got[len(got)-1])
		assert.Less(t, indexOf(got, "a"), indexOf(got, "c"))
		assert.Less(t, indexOf(got, "b"), indexOf(got, "c"))
	}))
}

func TestOrderedGroup_FlattenBlockPreservesSliceOrder(t *testing.T) {
	c := New()

	type flatOut struct {
		Out
		Values []int `group:"nums,flatten" grouptag:"block"`
	}
	require.NoError(t, c.Provide(func() flatOut {
		return flatOut{Values: []int{3, 4, 5}}
	}))
	require.NoError(t, c.Provide(func() struct {
		Out
		V int `group:"nums" grouptag:"first"`
	} {
		return struct {
			Out
			V int `group:"nums" grouptag:"first"`
		}{V: 1}
	}))

	type in struct {
		In
		Values []int `group:"nums,ordered" groupbefore:"first:block"`
	}

	require.NoError(t, c.Invoke(func(i in) {
		assert.Equal(t, []int{1, 3, 4, 5}, i.Values)
	}))
}

func TestOrderedGroup_EmptyFlattenResult(t *testing.T) {
	c := New()

	var ctorCalled int
	type flatOut struct {
		Out
		Values []int `group:"nums,flatten" grouptag:"empty"`
	}
	require.NoError(t, c.Provide(func() flatOut {
		ctorCalled++
		return flatOut{}
	}))
	require.NoError(t, c.Provide(func() struct {
		Out
		V int `group:"nums" grouptag:"one"`
	} {
		return struct {
			Out
			V int `group:"nums" grouptag:"one"`
		}{V: 42}
	}))

	type in struct {
		In
		Values []int `group:"nums,ordered" groupbefore:"empty:one"`
	}

	require.NoError(t, c.Invoke(func(i in) {
		assert.Equal(t, []int{42}, i.Values)
	}))
	assert.Equal(t, 1, ctorCalled)
}

func TestOrderedGroup_EmptyFlattenConflictsBeforeInvokingConsumer(t *testing.T) {
	c := New()

	type flatOut struct {
		Out
		Values []int `group:"nums,flatten" grouptag:"dup"`
	}
	require.NoError(t, c.Provide(func() flatOut { return flatOut{} }))
	require.NoError(t, c.Provide(func() struct {
		Out
		V int `group:"nums" grouptag:"dup"`
	} {
		t.Fatal("producer must not be called on ordering error")
		return struct {
			Out
			V int `group:"nums" grouptag:"dup"`
		}{}
	}))

	type in struct {
		In
		Values []int `group:"nums,ordered"`
	}
	consumerCalled := false
	err := c.Invoke(func(i in) { consumerCalled = true })
	require.Error(t, err)
	assert.Contains(t, err.Error(), `duplicate member tag "dup"`)
	assert.False(t, consumerCalled)
}

func TestOrderedGroup_SameCtorDifferentTags(t *testing.T) {
	c := New()

	// Two registrations of structurally identical closures with different tags.
	require.NoError(t, c.Provide(func() mw { return mw("first") },
		Group("mw"), GroupMemberTag("a")))
	require.NoError(t, c.Provide(func() mw { return mw("second") },
		Group("mw"), GroupMemberTag("b")))

	type in struct {
		In
		Middlewares []mw `group:"mw,ordered" groupbefore:"b:a"`
	}

	require.NoError(t, c.Invoke(func(i in) {
		assert.Equal(t, []string{"second", "first"}, names(i.Middlewares))
	}))
}

func TestOrderedGroup_MembersFromDifferentScopes(t *testing.T) {
	root := New()
	provideMW(root, "root-a", "root-a")

	child := root.Scope("child")
	provideMW(child, "child-b", "child-b")

	require.NoError(t, child.Invoke(func(i mwInOrdered) {
		got := names(i.Middlewares)
		assert.ElementsMatch(t, []string{"root-a", "child-b"}, got)
		// Root scope members register before child scope members.
		assert.Equal(t, "root-a", got[0])
	}))
}

func TestOrderedGroup_MembersFromDifferentScopesWithConstraints(t *testing.T) {
	root := New()
	provideMW(root, "audit", "audit")

	child := root.Scope("child")
	provideMW(child, "auth", "auth")
	provideMW(child, "recover", "recover")

	require.NoError(t, child.Invoke(func(i mwInConstraints) {
		got := names(i.Middlewares)
		assert.Equal(t, []string{"recover", "auth", "audit"}, got)
	}))
}

func TestOrderedGroup_DuplicateTagAcrossScopesConflicts(t *testing.T) {
	root := New()
	provideMW(root, "same", "same")

	child := root.Scope("child")
	provideMW(child, "same", "same")

	err := child.Invoke(func(i mwInOrdered) {})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `duplicate member tag "same"`)
}

func TestOrderedGroup_UnknownTagError(t *testing.T) {
	c := New()
	provideMW(c, "auth", "auth")

	type in struct {
		In
		Middlewares []mw `group:"mw,ordered" groupbefore:"auth:ghost"`
	}

	consumerCalled := false
	err := c.Invoke(func(i in) { consumerCalled = true })
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown member tag "ghost"`)
	assert.False(t, consumerCalled)
}

func TestOrderedGroup_CycleError(t *testing.T) {
	c := New()
	provideMW(c, "a", "a")
	provideMW(c, "b", "b")

	type in struct {
		In
		Middlewares []mw `group:"mw,ordered" groupbefore:"a:b" groupafter:"a:b"`
	}

	err := c.Invoke(func(i in) {})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cycle detected in ordering constraints")
}

func TestOrderedGroup_CycleDoesNotInvokeProducers(t *testing.T) {
	c := New()
	var calls int
	require.NoError(t, c.Provide(func() mw {
		calls++
		return mw("a")
	}, Group("mw"), GroupMemberTag("a")))
	require.NoError(t, c.Provide(func() mw {
		calls++
		return mw("b")
	}, Group("mw"), GroupMemberTag("b")))

	type in struct {
		In
		Middlewares []mw `group:"mw,ordered" groupbefore:"a:b" groupafter:"a:b"`
	}
	err := c.Invoke(func(i in) {})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cycle detected in ordering constraints")
	assert.Equal(t, 0, calls)
}

func TestOrderedGroup_AfterTagEquivalence(t *testing.T) {
	c := New()
	provideMW(c, "a", "a")
	provideMW(c, "b", "b")

	type in struct {
		In
		// a after b  => b before a
		Middlewares []mw `group:"mw,ordered" groupafter:"a:b"`
	}

	require.NoError(t, c.Invoke(func(i in) {
		assert.Equal(t, []string{"b", "a"}, names(i.Middlewares))
	}))
}

func TestUnorderedGroup_KeepsShuffledContract(t *testing.T) {
	// A tag declared on a producer must not affect unordered consumers, which
	// continue to receive members in an unspecified order.
	c := New(setRand(rand.New(rand.NewSource(1))))
	provideMW(c, "a", "a")
	provideMW(c, "b", "b")

	type in struct {
		In
		Middlewares []mw `group:"mw"`
	}

	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		require.NoError(t, c.Invoke(func(i in) {
			// Collect whatever order; membership is all that's guaranteed.
			assert.ElementsMatch(t, []string{"a", "b"}, names(i.Middlewares))
			seen[strings.Join(names(i.Middlewares), ",")] = true
		}))
	}
	// Membership is all that's guaranteed; the shuffle may produce either.
	assert.ElementsMatch(t, []string{"a,b", "b,a"}, keysOf(seen))
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestOrderedGroup_ConstraintsRequireOrdered(t *testing.T) {
	c := New()

	type in struct {
		In
		Middlewares []mw `group:"mw" groupbefore:"a:b"`
	}

	err := c.Invoke(func(i in) {})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "before/after constraints require an ordered value group")
}

func TestGroupTag_OnProducerErrors(t *testing.T) {
	c := New()

	type in struct {
		In
		Middlewares []mw `group:"mw,ordered" grouptag:"x"`
	}
	err := c.Invoke(func(i in) {})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `cannot use "grouptag" on a value-group consumer`)
}

func indexOf(xs []string, x string) int {
	for i, v := range xs {
		if v == x {
			return i
		}
	}
	return -1
}

type runner interface {
	Run() string
}

type namedRunner struct{ name string }

func (r namedRunner) Run() string { return r.name }

func TestOrderedGroup_AsType(t *testing.T) {
	c := New()
	require.NoError(t, c.Provide(func() namedRunner { return namedRunner{name: "a"} },
		Group("runners"), GroupMemberTag("a"), As(new(runner))))
	require.NoError(t, c.Provide(func() namedRunner { return namedRunner{name: "b"} },
		Group("runners"), GroupMemberTag("b"), As(new(runner))))

	type in struct {
		In
		Runners []runner `group:"runners,ordered" groupbefore:"b:a"`
	}
	require.NoError(t, c.Invoke(func(i in) {
		got := []string{i.Runners[0].Run(), i.Runners[1].Run()}
		assert.Equal(t, []string{"b", "a"}, got)
	}))
}

func TestOrderedGroup_EmptyGroup(t *testing.T) {
	c := New()
	require.NoError(t, c.Invoke(func(i mwInOrdered) {
		assert.Empty(t, i.Middlewares)
	}))
}

func TestOrderedGroup_PositionalFlattenOption(t *testing.T) {
	c := New()
	require.NoError(t, c.Provide(func() []int { return []int{7, 8, 9} },
		Group("nums,flatten"), GroupMemberTag("tail")))
	require.NoError(t, c.Provide(func() int { return 1 },
		Group("nums"), GroupMemberTag("head")))

	type in struct {
		In
		Values []int `group:"nums,ordered" groupbefore:"head:tail"`
	}
	require.NoError(t, c.Invoke(func(i in) {
		assert.Equal(t, []int{1, 7, 8, 9}, i.Values)
	}))
}

func TestOrderedGroup_TwoFlattenBlocksKeepSliceOrder(t *testing.T) {
	c := New()
	require.NoError(t, c.Provide(func() []int { return []int{3, 4} },
		Group("nums,flatten"), GroupMemberTag("b")))
	require.NoError(t, c.Provide(func() []int { return []int{1, 2} },
		Group("nums,flatten"), GroupMemberTag("a")))

	type in struct {
		In
		Values []int `group:"nums,ordered" groupbefore:"a:b"`
	}
	require.NoError(t, c.Invoke(func(i in) {
		assert.Equal(t, []int{1, 2, 3, 4}, i.Values)
	}))
}

func TestOrderedGroup_ProviderErrorStopsConsumer(t *testing.T) {
	c := New()
	provideMW(c, "ok", "ok")
	require.NoError(t, c.Provide(func() (mw, error) {
		return "", assert.AnError
	}, Group("mw"), GroupMemberTag("boom")))

	called := false
	err := c.Invoke(func(i mwInOrdered) { called = true })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not build value group")
	assert.False(t, called)
}

func TestOrderedGroup_DecoratedValuesStillCollected(t *testing.T) {
	c := New()
	provideMW(c, "raw", "raw")
	require.NoError(t, c.Decorate(func(in struct {
		In
		Values []mw `group:"mw"`
	}) struct {
		Out
		Values []mw `group:"mw" grouptag:"decorated"`
	} {
		require.Len(t, in.Values, 1)
		return struct {
			Out
			Values []mw `group:"mw" grouptag:"decorated"`
		}{Values: []mw{"wrapped"}}
	}))

	type in struct {
		In
		Values []mw `group:"mw,ordered"`
	}
	require.NoError(t, c.Invoke(func(i in) {
		assert.Equal(t, []string{"wrapped"}, names(i.Values))
	}))
}
