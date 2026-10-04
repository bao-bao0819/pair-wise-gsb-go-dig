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
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type orderedIn struct {
	In
	MWs []string `group:"server,ordered"`
}

func orderedContainer(t *testing.T) *Container {
	t.Helper()
	return New(setRand(rand.New(rand.NewSource(42))))
}

func provideMW(t *testing.T, c interface {
	Provide(interface{}, ...ProvideOption) error
}, value string, opts ...ProvideOption) {
	t.Helper()
	if len(opts) == 0 {
		opts = []ProvideOption{Group("server")}
	}
	require.NoError(t, c.Provide(func() string { return value }, opts...))
}

func consumeOrdered(t *testing.T, inv interface {
	Invoke(function interface{}, opts ...InvokeOption) error
}) []string {
	t.Helper()
	var got []string
	require.NoError(t, inv.Invoke(func(p orderedIn) {
		got = p.MWs
	}))
	return got
}

func TestOrderedGroup_BasicBeforeAfter(t *testing.T) {
	t.Parallel()

	c := orderedContainer(t)
	// Register in reverse of the desired order.
	provideMW(t, c, "audit", Group("server,label=audit,after=auth"))
	provideMW(t, c, "recover", Group("server,label=recover,before=auth"))
	provideMW(t, c, "auth", Group("server,label=auth,before=audit,after=recover"))

	assert.Equal(t, []string{"recover", "auth", "audit"}, consumeOrdered(t, c))
}

func TestOrderedGroup_Diamond(t *testing.T) {
	t.Parallel()

	c := orderedContainer(t)
	// recover sits outside every business middleware; every business
	// middleware sits before audit.
	provideMW(t, c, "recover", Group("server,label=recover"))
	provideMW(t, c, "audit", Group("server,label=audit"))
	provideMW(t, c, "auth", Group("server,label=auth,after=recover,before=audit"))
	provideMW(t, c, "metrics", Group("server,label=metrics,after=recover,before=audit"))

	got := consumeOrdered(t, c)
	require.Len(t, got, 4)
	assert.Equal(t, "recover", got[0])
	assert.Equal(t, "audit", got[len(got)-1])
	assert.ElementsMatch(t, []string{"auth", "metrics"}, got[1:3])
}

func TestOrderedGroup_UnlabeledStableByRegistration(t *testing.T) {
	t.Parallel()

	c := orderedContainer(t)
	provideMW(t, c, "one")
	provideMW(t, c, "auth", Group("server,label=auth,before=audit"))
	provideMW(t, c, "two")
	provideMW(t, c, "audit", Group("server,label=audit"))
	provideMW(t, c, "three")

	got := consumeOrdered(t, c)
	require.Len(t, got, 5)
	// The labeled pair honors its relationship: auth before audit.
	assert.Less(t, indexOf(got, "auth"), indexOf(got, "audit"))
	// Unconstrained members keep their relative registration order.
	loose := removeValues(got, "auth", "audit")
	assert.Equal(t, []string{"one", "two", "three"}, loose)
}

func TestOrderedGroup_ShuffledRegistrations(t *testing.T) {
	t.Parallel()

	for seed := int64(0); seed < 20; seed++ {
		c := New(setRand(rand.New(rand.NewSource(seed + 100))))

		defs := []struct {
			value, spec string
		}{
			{"audit", "server,label=audit,after=auth"},
			{"auth", "server,label=auth,after=recover"},
			{"recover", "server,label=recover"},
			{"loose-a", "server"},
			{"loose-b", "server"},
		}
		order := rand.New(rand.NewSource(seed)).Perm(len(defs))
		registered := make([]string, 0, len(defs))
		for _, idx := range order {
			provideMW(t, c, defs[idx].value, Group(defs[idx].spec))
			registered = append(registered, defs[idx].value)
		}

		got := consumeOrdered(t, c)
		require.Len(t, got, 5, "seed %d: %v", seed, got)
		// Explicit label relationships hold regardless of registration order.
		assert.Less(t, indexOf(got, "recover"), indexOf(got, "auth"), "seed %d", seed)
		assert.Less(t, indexOf(got, "auth"), indexOf(got, "audit"), "seed %d", seed)
		// The two unconstrained members keep their registration-relative
		// order, since no constraint relates them.
		if indexOf(registered, "loose-a") < indexOf(registered, "loose-b") {
			assert.Less(t, indexOf(got, "loose-a"), indexOf(got, "loose-b"), "seed %d", seed)
		} else {
			assert.Less(t, indexOf(got, "loose-b"), indexOf(got, "loose-a"), "seed %d", seed)
		}
	}
}

func TestOrderedGroup_FlattenContiguous(t *testing.T) {
	t.Parallel()

	c := orderedContainer(t)

	type flatOut struct {
		Out
		MWs []string `group:"server,flatten,label=batch"`
	}
	provideMW(t, c, "single", Group("server,label=single,after=batch"))
	require.NoError(t, c.Provide(func() flatOut {
		return flatOut{MWs: []string{"a", "b", "c"}}
	}))

	got := consumeOrdered(t, c)
	assert.Equal(t, []string{"a", "b", "c", "single"}, got)
}

func TestOrderedGroup_EmptyFlatten(t *testing.T) {
	t.Parallel()

	c := orderedContainer(t)

	type flatOut struct {
		Out
		MWs []string `group:"server,flatten,label=empty,before=afterme"`
	}
	require.NoError(t, c.Provide(func() flatOut {
		return flatOut{}
	}))
	provideMW(t, c, "afterme", Group("server,label=afterme"))
	provideMW(t, c, "beforeme", Group("server,label=beforeme,before=empty"))

	got := consumeOrdered(t, c)
	// Empty flatten contributes no values, but the other members still satisfy
	// their constraints across it.
	assert.Equal(t, []string{"beforeme", "afterme"}, got)
}

func TestOrderedGroup_SameFunctionDifferentLabels(t *testing.T) {
	t.Parallel()

	c := orderedContainer(t)

	require.NoError(t, c.Provide(func() string { return "first" },
		Group("server"), MemberLabel("first"), MemberBefore("second")))
	require.NoError(t, c.Provide(func() string { return "second" },
		Group("server"), MemberLabel("second"), MemberAfter("first")))

	assert.Equal(t, []string{"first", "second"}, consumeOrdered(t, c))
}

func TestOrderedGroup_TwoScopes(t *testing.T) {
	t.Parallel()

	c := orderedContainer(t)
	provideMW(t, c, "root", Group("server,label=root"))

	s := c.Scope("child")
	provideMW(t, s, "child", Group("server,label=child,before=root"))

	// The child sees both members and orders them by label across scopes.
	assert.Equal(t, []string{"child", "root"}, consumeOrdered(t, s))

	// The root scope does not see the child's member.
	assert.Equal(t, []string{"root"}, consumeOrdered(t, c))
}

func TestOrderedGroup_DuplicateLabelAcrossScopes(t *testing.T) {
	t.Parallel()

	c := orderedContainer(t)
	provideMW(t, c, "root", Group("server,label=dup"))

	s := c.Scope("child")
	provideMW(t, s, "child", Group("server,label=dup"))

	err := s.Invoke(func(p orderedIn) {})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `duplicate label "dup"`)
}

func TestOrderedGroup_UnknownLabel(t *testing.T) {
	t.Parallel()

	c := orderedContainer(t)
	provideMW(t, c, "auth", Group("server,label=auth,before=audit"))
	provideMW(t, c, "loose")

	err := c.Invoke(func(p orderedIn) {
		t.Fatal("consumer must not be called for unknown labels")
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown before label "audit"`)
}

func TestOrderedGroup_Cycle(t *testing.T) {
	t.Parallel()

	c := orderedContainer(t)
	provideMW(t, c, "a", Group("server,label=a,before=b"))
	provideMW(t, c, "b", Group("server,label=b,before=c"))
	provideMW(t, c, "c", Group("server,label=c,before=a"))

	err := c.Invoke(func(p orderedIn) {
		t.Fatal("consumer must not be called on a cycle")
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cycle detected in ordered value group")
}

func TestOrderedGroup_SelfConstraint(t *testing.T) {
	t.Parallel()

	c := orderedContainer(t)
	provideMW(t, c, "self", Group("server,label=self,before=self"))

	err := c.Invoke(func(p orderedIn) {
		t.Fatal("consumer must not be called on a self constraint")
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cycle detected in ordered value group")
}

func TestOrderedGroup_ConstructorsCalledOnce(t *testing.T) {
	t.Parallel()

	c := orderedContainer(t)
	calls := map[string]int{}

	tracked := func(value, spec string) {
		require.NoError(t, c.Provide(func() string {
			calls[value]++
			return value
		}, Group(spec)))
	}

	tracked("auth", "server,label=auth,before=audit")
	tracked("audit", "server,label=audit")

	consumeOrdered(t, c)
	consumeOrdered(t, c)

	assert.Equal(t, map[string]int{"auth": 1, "audit": 1}, calls)
}

func TestOrderedGroup_PreCalledSingleton(t *testing.T) {
	t.Parallel()

	c := orderedContainer(t)

	type holder string
	type marker struct{ name string }

	require.NoError(t, c.Provide(func() struct {
		Out
		Auth   marker
		Member holder `group:"server,label=auth,before=audit"`
	} {
		return struct {
			Out
			Auth   marker
			Member holder `group:"server,label=auth,before=audit"`
		}{
			Auth:   marker{name: "auth"},
			Member: "auth",
		}
	}))
	require.NoError(t, c.Provide(func() holder { return "audit" },
		Group("server"), MemberLabel("audit")))

	require.NoError(t, c.Invoke(func(m marker) {
		assert.Equal(t, "auth", m.name)
	}))

	var got []string
	require.NoError(t, c.Invoke(func(p struct {
		In
		MWs []holder `group:"server,ordered"`
	}) {
		for _, h := range p.MWs {
			got = append(got, string(h))
		}
	}))
	assert.Equal(t, []string{"auth", "audit"}, got)
}

func TestUnorderedGroup_KeepsContract(t *testing.T) {
	t.Parallel()

	// Labels and constraints on producers must not affect ordinary consumers.
	c := orderedContainer(t)
	provideMW(t, c, "audit", Group("server,label=audit,after=auth"))
	provideMW(t, c, "auth", Group("server,label=auth,before=audit"))

	var got []string
	require.NoError(t, c.Invoke(func(p struct {
		In
		MWs []string `group:"server"`
	}) {
		got = p.MWs
	}))
	assert.ElementsMatch(t, []string{"auth", "audit"}, got)
}

func TestOrderedGroup_RejectsProducerOrdered(t *testing.T) {
	t.Parallel()

	c := New()
	err := c.Provide(func() string { return "x" }, Group("server,ordered"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot use ordered with result value groups")
}

func TestOrderedGroup_RejectsConsumerLabels(t *testing.T) {
	t.Parallel()

	c := New()
	provideMW(t, c, "x")

	err := c.Invoke(func(p struct {
		In
		MWs []string `group:"server,ordered,label=foo"`
	}) {
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "member labels are not allowed when consuming")
}

func TestOrderedGroup_MemberOptionsWithoutGroup(t *testing.T) {
	t.Parallel()

	c := New()
	err := c.Provide(func() string { return "x" }, MemberLabel("x"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "member ordering options require dig.Group")
}

func TestOrderedGroup_EmptyGroup(t *testing.T) {
	t.Parallel()

	c := orderedContainer(t)
	got := consumeOrdered(t, c)
	assert.Empty(t, got)
}

func TestOrderedGroup_SoftConsumesExistingValues(t *testing.T) {
	t.Parallel()

	c := orderedContainer(t)
	provideMW(t, c, "auth", Group("server,label=auth,before=audit"))
	provideMW(t, c, "audit", Group("server,label=audit"))

	type softIn struct {
		In
		Soft []string `group:"server,soft,ordered"`
	}

	// With nothing else pulling the group, a soft ordered group must not
	// execute its providers on its own.
	require.NoError(t, c.Provide(func(p softIn) string {
		return "soft-consumer"
	}))
	require.NoError(t, c.Invoke(func(v string) {
		assert.Equal(t, "soft-consumer", v)
	}))

	// But when the members are executed for another consumer, a later soft
	// ordered consumer sees the values in the declared order.
	consumeOrdered(t, c)
	var got []string
	require.NoError(t, c.Invoke(func(p softIn) {
		got = p.Soft
	}))
	assert.Equal(t, []string{"auth", "audit"}, got)
}

func indexOf(items []string, want string) int {
	for i, item := range items {
		if item == want {
			return i
		}
	}
	return -1
}

func removeValues(items []string, remove ...string) []string {
	skip := make(map[string]struct{}, len(remove))
	for _, r := range remove {
		skip[r] = struct{}{}
	}
	var out []string
	for _, item := range items {
		if _, ok := skip[item]; ok {
			continue
		}
		out = append(out, item)
	}
	return out
}
