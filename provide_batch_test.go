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
	"bytes"
	"fmt"
	"io"
	"reflect"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scopeSnapshot captures the externally-observable registration state of a
// Scope subtree so that two Containers can be compared after different
// sequences of Provide/ProvideBatch calls.
type scopeSnapshot struct {
	name              string
	nodeOrders        map[reflect.Type]int
	graphNodes        int
	providers         map[string]int
	isVerifiedAcyclic bool
	children          []scopeSnapshot
}

func snapshotScope(s *Scope) scopeSnapshot {
	snap := scopeSnapshot{
		name:              s.name,
		nodeOrders:        make(map[reflect.Type]int),
		graphNodes:        len(s.gh.nodes),
		providers:         make(map[string]int),
		isVerifiedAcyclic: s.isVerifiedAcyclic,
	}

	for _, n := range s.gh.nodes {
		if cn, ok := n.Wrapped.(*constructorNode); ok {
			snap.nodeOrders[cn.CType()] = cn.Order(s)
		}
	}

	for k, nodes := range s.providers {
		snap.providers[k.String()] = len(nodes)
	}

	for _, cs := range s.childScopes {
		snap.children = append(snap.children, snapshotScope(cs))
	}
	return snap
}

func assertScopeSnapshotsEqual(t *testing.T, want, got scopeSnapshot) {
	t.Helper()

	assert.Equal(t, want.name, got.name, "scope name")
	assert.Equal(t, want.isVerifiedAcyclic, got.isVerifiedAcyclic,
		"isVerifiedAcyclic for scope %q", got.name)
	assert.Equal(t, want.graphNodes, got.graphNodes,
		"graph node count in scope %q", got.name)

	// The set of registered constructor types must match.
	assert.Equal(t, typeSet(want.nodeOrders), typeSet(got.nodeOrders),
		"constructor types in scope %q", got.name)

	// Relative ordering of the graph nodes must match, which guarantees that
	// a retried batch occupies the same graph positions as a batch that
	// never failed.
	assert.Equal(t, normalizeOrders(want.nodeOrders), normalizeOrders(got.nodeOrders),
		"normalized node orders in scope %q", got.name)

	assert.Equal(t, want.providers, got.providers, "providers in scope %q", got.name)

	require.Len(t, got.children, len(want.children),
		"child scope count for scope %q", got.name)
	for i := range want.children {
		assertScopeSnapshotsEqual(t, want.children[i], got.children[i])
	}
}

func typeSet(m map[reflect.Type]int) []reflect.Type {
	types := make([]reflect.Type, 0, len(m))
	for t := range m {
		types = append(types, t)
	}
	sort.Slice(types, func(i, j int) bool {
		return types[i].String() < types[j].String()
	})
	return types
}

// normalizeOrders maps the order values onto a dense, sorted ranking so the
// comparison ignores differences in absolute node IDs between containers.
func normalizeOrders(m map[reflect.Type]int) map[reflect.Type]int {
	orders := make([]int, 0, len(m))
	for _, o := range m {
		orders = append(orders, o)
	}
	sort.Ints(orders)

	rank := make(map[int]int, len(orders))
	for i, o := range orders {
		rank[o] = i
	}

	out := make(map[reflect.Type]int, len(m))
	for t, o := range m {
		out[t] = rank[o]
	}
	return out
}

func TestProvideBatchSuccess(t *testing.T) {
	t.Parallel()

	t.Run("ordinary dependencies", func(t *testing.T) {
		t.Parallel()

		type config struct{ name string }
		type service struct{ cfg *config }

		c := New()
		err := c.ProvideBatch(
			BatchProvider{Constructor: func() *config { return &config{name: "cfg"} }},
			BatchProvider{Constructor: func(cfg *config) *service { return &service{cfg: cfg} }},
		)
		require.NoError(t, err)

		require.NoError(t, c.Invoke(func(svc *service) {
			assert.Equal(t, "cfg", svc.cfg.name)
		}))
	})

	t.Run("front of batch depends on back of batch", func(t *testing.T) {
		t.Parallel()

		type back struct{}
		type mid struct{ b *back }
		type front struct{ m *mid }

		c := New()
		err := c.ProvideBatch(
			BatchProvider{Constructor: func(m *mid) *front { return &front{m: m} }},
			BatchProvider{Constructor: func(b *back) *mid { return &mid{b: b} }},
			BatchProvider{Constructor: func() *back { return &back{} }},
		)
		require.NoError(t, err)

		require.NoError(t, c.Invoke(func(f *front) {
			assert.NotNil(t, f.m.b)
		}))
	})

	t.Run("named values", func(t *testing.T) {
		t.Parallel()

		type conn struct{ kind string }
		type params struct {
			In

			RO *conn `name:"ro"`
			RW *conn `name:"rw"`
		}

		c := New()
		err := c.ProvideBatch(
			BatchProvider{
				Constructor: func() *conn { return &conn{kind: "ro"} },
				Options:     []ProvideOption{Name("ro")},
			},
			BatchProvider{
				Constructor: func() *conn { return &conn{kind: "rw"} },
				Options:     []ProvideOption{Name("rw")},
			},
		)
		require.NoError(t, err)

		require.NoError(t, c.Invoke(func(p params) {
			assert.Equal(t, "ro", p.RO.kind)
			assert.Equal(t, "rw", p.RW.kind)
		}))
	})

	t.Run("value groups", func(t *testing.T) {
		t.Parallel()

		type plugin struct{ name string }
		type params struct {
			In

			Plugins []*plugin `group:"plugins"`
		}

		c := New()
		err := c.ProvideBatch(
			BatchProvider{
				Constructor: func() *plugin { return &plugin{name: "a"} },
				Options:     []ProvideOption{Group("plugins")},
			},
			BatchProvider{
				Constructor: func() *plugin { return &plugin{name: "b"} },
				Options:     []ProvideOption{Group("plugins")},
			},
			// A consumer of the group may itself live in the same batch and
			// reference producers appearing before and after it.
			BatchProvider{
				Constructor: func(p params) []string {
					names := make([]string, len(p.Plugins))
					for i, pl := range p.Plugins {
						names[i] = pl.name
					}
					sort.Strings(names)
					return names
				},
			},
		)
		require.NoError(t, err)

		require.NoError(t, c.Invoke(func(names []string) {
			assert.Equal(t, []string{"a", "b"}, names)
		}))
	})

	t.Run("As propagation", func(t *testing.T) {
		t.Parallel()

		type result struct {
			*bytes.Buffer
		}

		c := New()
		err := c.ProvideBatch(
			BatchProvider{
				Constructor: func() *bytes.Buffer { return new(bytes.Buffer) },
				Options:     []ProvideOption{As(new(io.Reader), new(io.Writer))},
			},
			BatchProvider{Constructor: func(r io.Reader, w io.Writer) result {
				return result{Buffer: r.(*bytes.Buffer)}
			}},
		)
		require.NoError(t, err)

		// The concrete type must not be available; only the As interfaces.
		err = c.Invoke(func(b *bytes.Buffer) {})
		require.Error(t, err)

		require.NoError(t, c.Invoke(func(r result) {
			assert.NotNil(t, r.Buffer)
		}))
	})

	t.Run("Export propagates to child scopes but not siblings", func(t *testing.T) {
		t.Parallel()

		type exported struct{}
		type private struct{}

		c := New()
		child1 := c.Scope("child1")
		child2 := c.Scope("child2")
		grandchild := child1.Scope("grandchild")

		require.NoError(t, child1.ProvideBatch(
			BatchProvider{
				Constructor: func() *exported { return &exported{} },
				Options:     []ProvideOption{Export(true)},
			},
			BatchProvider{Constructor: func() *private { return &private{} }},
		))

		// Exported constructor lands on the root scope.
		require.Len(t, c.scope.nodes, 1)
		require.NoError(t, c.Invoke(func(*exported) {}))
		require.NoError(t, child1.Invoke(func(*exported) {}))
		require.NoError(t, child2.Invoke(func(*exported) {}))
		require.NoError(t, grandchild.Invoke(func(*exported) {}))

		// Private constructor stays on child1.
		require.NoError(t, child1.Invoke(func(*private) {}))
		require.NoError(t, grandchild.Invoke(func(*private) {}))
		require.Error(t, c.Invoke(func(*private) {}))
		require.Error(t, child2.Invoke(func(*private) {}))
	})

	t.Run("FillProvideInfo is populated per constructor", func(t *testing.T) {
		t.Parallel()

		type dep struct{}
		type svc struct{ d *dep }

		var depInfo, svcInfo ProvideInfo
		c := New()
		require.NoError(t, c.ProvideBatch(
			BatchProvider{
				Constructor: func() *dep { return &dep{} },
				Options:     []ProvideOption{FillProvideInfo(&depInfo)},
			},
			BatchProvider{
				Constructor: func(d *dep) *svc { return &svc{d: d} },
				Options:     []ProvideOption{FillProvideInfo(&svcInfo)},
			},
		))

		require.NotZero(t, depInfo.ID)
		require.NotZero(t, svcInfo.ID)
		require.NotEqual(t, depInfo.ID, svcInfo.ID)
		require.Empty(t, depInfo.Inputs)
		require.Len(t, depInfo.Outputs, 1)
		require.Len(t, svcInfo.Inputs, 1)
		require.Len(t, svcInfo.Outputs, 1)
		assert.Equal(t, "*dig.dep", svcInfo.Inputs[0].String())
	})

	t.Run("empty batch is a no-op", func(t *testing.T) {
		t.Parallel()

		c := New()
		before := snapshotScope(c.scope)
		require.NoError(t, c.ProvideBatch())
		assertScopeSnapshotsEqual(t, before, snapshotScope(c.scope))
	})
}

func TestProvideBatchFailureAtomicity(t *testing.T) {
	t.Parallel()

	// assertUnchanged fails the test if the scope tree differs from the
	// snapshot taken before the failed batch.
	assertUnchanged := func(t *testing.T, before scopeSnapshot, c *Container) {
		t.Helper()
		assertScopeSnapshotsEqual(t, before, snapshotScope(c.scope))
	}

	t.Run("duplicate type inside the batch", func(t *testing.T) {
		t.Parallel()

		type dup struct{}

		c := New()
		before := snapshotScope(c.scope)
		err := c.ProvideBatch(
			BatchProvider{Constructor: func() *dup { return &dup{} }},
			BatchProvider{Constructor: func() *dup { return &dup{} }},
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already provided")
		assertUnchanged(t, before, c)

		// One provider of the type still works after the rollback.
		require.NoError(t, c.ProvideBatch(
			BatchProvider{Constructor: func() *dup { return &dup{} }},
		))
		require.NoError(t, c.Invoke(func(*dup) {}))
	})

	t.Run("conflict with pre-existing provider rolls back the whole batch", func(t *testing.T) {
		t.Parallel()

		type existing struct{}
		type fresh struct{}

		c := New()
		require.NoError(t, c.Provide(func() *existing { return &existing{} }))
		before := snapshotScope(c.scope)

		err := c.ProvideBatch(
			BatchProvider{Constructor: func() *fresh { return &fresh{} }},
			BatchProvider{Constructor: func() *existing { return &existing{} }},
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already provided")
		assertUnchanged(t, before, c)

		// The first, non-conflicting constructor must not be usable.
		require.Error(t, c.Invoke(func(*fresh) {}))
	})

	t.Run("multi-output constructor conflicts on only one output", func(t *testing.T) {
		t.Parallel()

		type good struct{}
		type conflict struct{}
		type other struct{}

		type twoOut struct {
			Out

			G *good
			C *conflict
		}

		c := New()
		require.NoError(t, c.Provide(func() *conflict { return &conflict{} }))
		require.NoError(t, c.Provide(func() *other { return &other{} }))
		before := snapshotScope(c.scope)

		err := c.ProvideBatch(
			BatchProvider{Constructor: func(o *other) *good {
				return &good{}
			}},
			BatchProvider{Constructor: func() twoOut { return twoOut{} }},
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already provided")
		assertUnchanged(t, before, c)

		// Neither output of the multi-output constructor nor the first
		// constructor in the batch survived the rollback.
		require.Error(t, c.Invoke(func(*good) {}))
		require.NoError(t, c.Invoke(func(*other) {}))
	})

	t.Run("cycle inside the batch rolls everything back", func(t *testing.T) {
		t.Parallel()

		type a struct{}
		type b struct{}
		type ctype struct{}

		c := New()
		before := snapshotScope(c.scope)
		err := c.ProvideBatch(
			BatchProvider{Constructor: func(*ctype) *a { return &a{} }},
			BatchProvider{Constructor: func(*a) *b { return &b{} }},
			BatchProvider{Constructor: func(*b) *ctype { return &ctype{} }},
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cycle")
		assertUnchanged(t, before, c)

		require.Error(t, c.Invoke(func(*a) {}))
	})

	t.Run("invalid constructor before registration mutates nothing", func(t *testing.T) {
		t.Parallel()

		type good struct{}

		c := New()
		before := snapshotScope(c.scope)

		require.Error(t, c.ProvideBatch(
			BatchProvider{Constructor: func() *good { return &good{} }},
			BatchProvider{Constructor: (*good)(nil)},
		))
		assertUnchanged(t, before, c)
		require.Error(t, c.Invoke(func(*good) {}))
	})

	t.Run("untyped nil constructor mutates nothing", func(t *testing.T) {
		t.Parallel()

		c := New()
		before := snapshotScope(c.scope)
		err := c.ProvideBatch(BatchProvider{Constructor: nil})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "untyped nil")
		assertUnchanged(t, before, c)
	})

	t.Run("non-function constructor mutates nothing", func(t *testing.T) {
		t.Parallel()

		c := New()
		before := snapshotScope(c.scope)
		err := c.ProvideBatch(BatchProvider{Constructor: 42})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must provide constructor function")
		assertUnchanged(t, before, c)
	})

	t.Run("invalid options mutate nothing", func(t *testing.T) {
		t.Parallel()

		type good struct{}

		c := New()
		before := snapshotScope(c.scope)
		err := c.ProvideBatch(
			BatchProvider{
				Constructor: func() *good { return &good{} },
				Options:     []ProvideOption{Name("n"), Group("g")},
			},
		)
		require.Error(t, err)
		assertUnchanged(t, before, c)
		require.Error(t, c.Invoke(func(*good) {}))
	})

	t.Run("constructor with no results mutates nothing", func(t *testing.T) {
		t.Parallel()

		type good struct{}

		c := New()
		before := snapshotScope(c.scope)
		err := c.ProvideBatch(
			BatchProvider{Constructor: func() *good { return &good{} }},
			BatchProvider{Constructor: func() {}},
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must provide at least one non-error type")
		assertUnchanged(t, before, c)
		require.Error(t, c.Invoke(func(*good) {}))
	})

	t.Run("rollback restores pre-existing provider entries", func(t *testing.T) {
		t.Parallel()

		type existing struct{}
		type fresh struct{}

		c := New()
		existingCtor := func() *existing { return &existing{} }
		require.NoError(t, c.Provide(existingCtor))

		// Invoke so the pre-existing provider has a produced value cached.
		require.NoError(t, c.Invoke(func(*existing) {}))

		before := snapshotScope(c.scope)
		err := c.ProvideBatch(
			BatchProvider{Constructor: func() *fresh { return &fresh{} }},
			BatchProvider{Constructor: func() *existing { return &existing{} }},
		)
		require.Error(t, err)
		assertScopeSnapshotsEqual(t, before, snapshotScope(c.scope))

		// The original provider must remain the single provider and still
		// resolve.
		require.Len(t, c.scope.providers[key{t: reflect.TypeOf(&existing{})}], 1)
		require.NoError(t, c.Invoke(func(*existing) {}))
		require.Error(t, c.Invoke(func(*fresh) {}))
	})

	t.Run("cycle failure restores pre-existing provider entries", func(t *testing.T) {
		t.Parallel()

		type seed struct{}
		type dup struct{}

		c := New()
		require.NoError(t, c.Provide(func() *seed { return &seed{} }, Group("seeds")))
		before := snapshotScope(c.scope)

		// The batch appends another element to the pre-existing value group
		// entry and then fails on a duplicate registration, so rollback must
		// restore the old providers slice in place.
		err := c.ProvideBatch(
			BatchProvider{
				Constructor: func() *seed { return &seed{} },
				Options:     []ProvideOption{Group("seeds")},
			},
			BatchProvider{Constructor: func() *dup { return &dup{} }},
			BatchProvider{Constructor: func() *dup { return &dup{} }},
		)
		require.Error(t, err)
		assertScopeSnapshotsEqual(t, before, snapshotScope(c.scope))
		groupKey := key{group: "seeds", t: reflect.TypeOf(&seed{})}
		require.Len(t, c.scope.providers[groupKey], 1)
	})

	t.Run("bad result options roll back earlier graph nodes", func(t *testing.T) {
		t.Parallel()

		type good struct{}
		type bad struct{}

		type out struct {
			Out

			B *bad `group:"g" name:"n"`
		}

		c := New()
		before := snapshotScope(c.scope)
		err := c.ProvideBatch(
			BatchProvider{Constructor: func() *good { return &good{} }},
			BatchProvider{Constructor: func() out { return out{} }},
		)
		require.Error(t, err)
		assertUnchanged(t, before, c)
	})

	t.Run("FillProvideInfo from a failed batch stays zero", func(t *testing.T) {
		t.Parallel()

		type dup struct{}

		var firstInfo, secondInfo ProvideInfo
		c := New()
		before := snapshotScope(c.scope)
		err := c.ProvideBatch(
			BatchProvider{
				Constructor: func() *dup { return &dup{} },
				Options:     []ProvideOption{FillProvideInfo(&firstInfo)},
			},
			BatchProvider{
				Constructor: func() *dup { return &dup{} },
				Options:     []ProvideOption{FillProvideInfo(&secondInfo)},
			},
		)
		require.Error(t, err)
		assert.Zero(t, firstInfo.ID)
		assert.Empty(t, firstInfo.Inputs)
		assert.Empty(t, firstInfo.Outputs)
		assert.Zero(t, secondInfo.ID)
		assertUnchanged(t, before, c)
	})

	t.Run("cycle crossing parent and child scopes", func(t *testing.T) {
		t.Parallel()

		type a struct{}
		type b struct{}
		type ctype struct{}

		// Root provides A <- C; the batch provides B <- A on the child and
		// C <- B exported to the root, closing the loop in every scope.
		setup := func() (*Container, *Scope) {
			c := New()
			child := c.Scope("child")
			require.NoError(t, c.Provide(func(*ctype) *a { return &a{} }))
			return c, child
		}

		c, child := setup()
		beforeC := snapshotScope(c.scope)

		err := child.ProvideBatch(
			BatchProvider{Constructor: func(*a) *b { return &b{} }},
			BatchProvider{
				Constructor: func(*b) *ctype { return &ctype{} },
				Options:     []ProvideOption{Export(true)},
			},
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cycle")
		assert.Contains(t, err.Error(), `[scope "child"]`)
		assertScopeSnapshotsEqual(t, beforeC, snapshotScope(c.scope))

		// Fix the cycle: replace the exported C constructor with one that
		// does not depend on B. The corrected batch must succeed and behave
		// like a container that never saw the failing batch.
		c2, child2 := setup()
		require.NoError(t, child2.ProvideBatch(
			BatchProvider{Constructor: func(*a) *b { return &b{} }},
			BatchProvider{
				Constructor: func() *ctype { return &ctype{} },
				Options:     []ProvideOption{Export(true)},
			},
		))
		require.NoError(t, child2.Invoke(func(*b) {}))
		require.NoError(t, c2.Invoke(func(*a) {}))
	})

	t.Run("DeferAcyclicVerification still checks the batch", func(t *testing.T) {
		t.Parallel()

		type a struct{}
		type b struct{}

		c := New(DeferAcyclicVerification())
		assert.False(t, c.scope.isVerifiedAcyclic)
		before := snapshotScope(c.scope)

		err := c.ProvideBatch(
			BatchProvider{Constructor: func(*b) *a { return &a{} }},
			BatchProvider{Constructor: func(*a) *b { return &b{} }},
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cycle")
		assertScopeSnapshotsEqual(t, before, snapshotScope(c.scope))

		// A valid batch succeeds and marks the graph verified.
		require.NoError(t, c.ProvideBatch(
			BatchProvider{Constructor: func() *a { return &a{} }},
			BatchProvider{Constructor: func(*a) *b { return &b{} }},
		))
		assert.True(t, c.scope.isVerifiedAcyclic)
	})

	t.Run("deferred container with pre-existing cycle fails new batch", func(t *testing.T) {
		t.Parallel()

		type a struct{}
		type b struct{}
		type fresh struct{}

		c := New(DeferAcyclicVerification())
		// Single Provide keeps its original semantics: deferred cycle
		// checks do not run.
		require.NoError(t, c.Provide(func(*b) *a { return &a{} }))
		require.NoError(t, c.Provide(func(*a) *b { return &b{} }))

		before := snapshotScope(c.scope)
		err := c.ProvideBatch(
			BatchProvider{Constructor: func() *fresh { return &fresh{} }},
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cycle")
		assertScopeSnapshotsEqual(t, before, snapshotScope(c.scope))
	})
}

func TestProvideBatchZeroExecution(t *testing.T) {
	t.Parallel()

	t.Run("successful registration never calls constructors", func(t *testing.T) {
		t.Parallel()

		var count int
		inc := func() int { count++; return count }

		c := New()
		require.NoError(t, c.ProvideBatch(
			BatchProvider{Constructor: func() int { return inc() }},
			BatchProvider{Constructor: func(n int) string {
				return fmt.Sprintf("n=%d", n)
			}},
		))
		assert.Zero(t, count, "constructors must not run during registration")

		require.NoError(t, c.Invoke(func(string) {}))
		assert.Equal(t, 1, count)
	})

	t.Run("failed registration never calls any constructor", func(t *testing.T) {
		t.Parallel()

		var count int
		type a struct{}
		type b struct{}

		c := New()
		err := c.ProvideBatch(
			BatchProvider{Constructor: func() *a { count++; return &a{} }},
			BatchProvider{Constructor: func(*a) *b { count++; return &b{} }},
			BatchProvider{Constructor: func(*b) *a { count++; return &a{} }},
		)
		require.Error(t, err)
		assert.Zero(t, count, "no constructor in a failed batch may run")
	})

	t.Run("conflicting batch with group consumer adds no group node", func(t *testing.T) {
		t.Parallel()

		type item struct{}
		type dup struct{}
		type params struct {
			In

			Items []*item `group:"items"`
		}

		var count int
		c := New()
		before := snapshotScope(c.scope)
		err := c.ProvideBatch(
			BatchProvider{
				Constructor: func() *item { count++; return &item{} },
				Options:     []ProvideOption{Group("items")},
			},
			BatchProvider{Constructor: func(params) int { count++; return 1 }},
			BatchProvider{Constructor: func() *dup { count++; return &dup{} }},
			BatchProvider{Constructor: func() *dup { count++; return &dup{} }},
		)
		require.Error(t, err)
		assert.Zero(t, count)
		assertScopeSnapshotsEqual(t, before, snapshotScope(c.scope))
	})
}

func TestProvideBatchDoesNotTouchValueCaches(t *testing.T) {
	t.Parallel()

	type cached struct{}
	type fresh struct{}
	type dup struct{}

	c := New()
	require.NoError(t, c.Provide(func() *cached { return &cached{} }))
	var cachedValue *cached
	require.NoError(t, c.Invoke(func(v *cached) { cachedValue = v }))
	require.NotNil(t, cachedValue)

	// Snapshot the value caches directly: a failed batch must not add or
	// replace any produced or grouped values.
	valuesBefore := len(c.scope.values)
	groupsBefore := len(c.scope.groups)

	err := c.ProvideBatch(
		BatchProvider{Constructor: func() *fresh { return &fresh{} }},
		BatchProvider{Constructor: func() *dup { return &dup{} }},
		BatchProvider{Constructor: func() *dup { return &dup{} }},
	)
	require.Error(t, err)

	assert.Equal(t, valuesBefore, len(c.scope.values))
	assert.Equal(t, groupsBefore, len(c.scope.groups))
	require.NoError(t, c.Invoke(func(v *cached) {
		assert.Same(t, cachedValue, v, "cached value must be unchanged")
	}))
}

func TestProvideBatchRetryEquivalence(t *testing.T) {
	t.Parallel()

	// Build two structurally identical scope trees. `failing` first
	// attempts a bad batch (a cycle), rolls it back, and then submits a
	// corrected batch. `clean` only submits the corrected batch.
	type a struct{}
	type b struct{}
	type ctype struct{}

	build := func() (*Container, *Scope, *Scope) {
		c := New()
		child := c.Scope("child")
		sibling := c.Scope("sibling")
		return c, child, sibling
	}

	// corrected: A has no dependencies, B depends on A, C depends on B.
	corrected := func() []BatchProvider {
		return []BatchProvider{
			{Constructor: func() *a { return &a{} }},
			{Constructor: func(*a) *b { return &b{} }},
			{Constructor: func(*b) *ctype { return &ctype{} }},
		}
	}

	failingC, failingChild, _ := build()
	// Seed both containers with an identical pre-existing provider.
	require.NoError(t, failingC.Provide(func() string { return "seed" }))

	err := failingChild.ProvideBatch(
		BatchProvider{Constructor: func(*ctype) *a { return &a{} }},
		BatchProvider{Constructor: func(*a) *b { return &b{} }},
		BatchProvider{Constructor: func(*b) *ctype { return &ctype{} }},
	)
	require.Error(t, err)
	require.NoError(t, failingChild.ProvideBatch(corrected()...))

	cleanC, cleanChild, _ := build()
	require.NoError(t, cleanC.Provide(func() string { return "seed" }))
	require.NoError(t, cleanChild.ProvideBatch(corrected()...))

	assertScopeSnapshotsEqual(t, snapshotScope(cleanC.scope), snapshotScope(failingC.scope))

	// Both containers must resolve the full chain identically.
	for _, pair := range []struct {
		name  string
		c     *Container
		child *Scope
	}{
		{name: "failing", c: failingC, child: failingChild},
		{name: "clean", c: cleanC, child: cleanChild},
	} {
		require.NoError(t, pair.child.Invoke(func(v *ctype) {
			assert.NotNil(t, v, pair.name)
		}))
		require.Error(t, pair.c.Invoke(func(v *ctype) {}),
			"private providers must not leak to root in %s", pair.name)
	}
}

func TestProvideSingleUnchanged(t *testing.T) {
	t.Parallel()

	// A simple sanity test ensuring Provide still works after the batch
	// entry point was added, including cycle rollback semantics.
	type a struct{}
	type b struct{}

	c := New(DeferAcyclicVerification())
	require.NoError(t, c.Provide(func(*b) *a { return &a{} }))
	require.NoError(t, c.Provide(func(*a) *b { return &b{} }))

	// Deferred single Provide retains its original semantics; the cycle is
	// only surfaced at Invoke time.
	err := c.Invoke(func(*a) {})
	require.Error(t, err)
}
