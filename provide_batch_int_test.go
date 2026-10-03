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

package dig

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type intBatchA struct{}
type intBatchB struct{}

func graphNodeCount(s *Scope) int { return len(s.gh.nodes) }

// A failed batch must not leave providers, graph nodes, Scope.nodes entries
// or flip the acyclic-verification flag, even when the batch targeted a
// grandchild and propagated exports through pre-existing Scopes.
func TestProvideBatchFailureLeavesNoTrace(t *testing.T) {
	root := New()
	rs := root.scope
	c1 := rs.Scope("child1")
	gc1 := c1.Scope("grandchild1")
	c2 := rs.Scope("child2")

	// Pre-existing constructor on the root; this provider must remain.
	require.NoError(t, rs.Provide(func() int { return 1 }))
	// Force the graphs into a verified state.
	require.NoError(t, rs.Invoke(func(int) {}))

	allScopes := []*Scope{rs, c1, gc1, c2}

	type before struct {
		providers int
		nodes     int
		graph     int
		verified  bool
	}
	snapshots := make(map[*Scope]before, len(allScopes))
	for _, sc := range allScopes {
		snapshots[sc] = before{
			providers: len(sc.providers),
			nodes:     len(sc.nodes),
			graph:     graphNodeCount(sc),
			verified:  sc.isVerifiedAcyclic,
		}
	}

	// A batch submitted to gc1: one private constructor and one exported
	// constructor. The exported one clashes with the existing int provider
	// on the root, so the whole batch must fail.
	err := gc1.ProvideBatch(
		ProvideSpec{Constructor: func() intBatchA { return intBatchA{} }},
		ProvideSpec{
			Constructor: func() int { return 2 },
			Options:     []ProvideOption{Export(true)},
		},
	)
	require.Error(t, err)

	for _, sc := range allScopes {
		want := snapshots[sc]
		assert.Equal(t, want.providers, len(sc.providers), "%s providers", sc.name)
		assert.Equal(t, want.nodes, len(sc.nodes), "%s nodes", sc.name)
		assert.Equal(t, want.graph, graphNodeCount(sc), "%s graph nodes", sc.name)
		assert.Equal(t, want.verified, sc.isVerifiedAcyclic, "%s verified", sc.name)
		assert.Equal(t, -1, sc.gh.snap, "%s snapshot not committed/rolled back", sc.name)
	}

	// The new types are resolvable nowhere.
	assert.Error(t, rs.Invoke(func(intBatchA) {}))
	assert.Error(t, c2.Invoke(func(intBatchA) {}))
	// The original provider still works.
	require.NoError(t, rs.Invoke(func(i int) { assert.Equal(t, 1, i) }))
}

// A failed batch in a deferred container must also restore the (unverified)
// flag state rather than leaving it verified.
func TestProvideBatchFailureRestoresDeferredFlag(t *testing.T) {
	c := New(DeferAcyclicVerification())
	root := c.scope

	require.NoError(t, root.Provide(func() int { return 1 }))
	assert.False(t, root.isVerifiedAcyclic)

	err := root.ProvideBatch(
		ProvideSpec{Constructor: func() intBatchB { return intBatchB{} }},
		ProvideSpec{Constructor: func() int { return 2 }},
	)
	require.Error(t, err)

	assert.False(t, root.isVerifiedAcyclic)
	assert.Equal(t, 1, len(root.providers))
	assert.Equal(t, 1, len(root.nodes))
}

// Containers that suffered a failed batch and then received the corrected
// batch must be internally identical to one that only saw the correction.
func TestProvideBatchRetryStateEquivalence(t *testing.T) {
	build := func(seedFailure bool) *Scope {
		c := New()
		s := c.scope
		require.NoError(t, s.Provide(func() int { return 1 }))

		if seedFailure {
			err := s.ProvideBatch(
				ProvideSpec{Constructor: func() string { return "bad" }},
				ProvideSpec{Constructor: func() int { return 2 }},
			)
			require.Error(t, err)
		}

		require.NoError(t, s.ProvideBatch(
			ProvideSpec{Constructor: func() string { return "good" }},
			ProvideSpec{Constructor: func(r *strings.Reader) intBatchB {
				r.Len()
				return intBatchB{}
			}},
			ProvideSpec{Constructor: func() *strings.Reader {
				return strings.NewReader("x")
			}},
		))
		return s
	}

	failed := build(true)
	clean := build(false)

	assert.Equal(t, len(clean.providers), len(failed.providers))
	assert.Equal(t, len(clean.nodes), len(failed.nodes))
	assert.Equal(t, graphNodeCount(clean), graphNodeCount(failed))

	// Same set of provided keys and the same number of constructors each.
	keySets := func(s *Scope) map[key]int {
		m := make(map[key]int, len(s.providers))
		for k, ps := range s.providers {
			m[k] = len(ps)
		}
		return m
	}
	wantKeys := keySets(clean)
	gotKeys := keySets(failed)
	assert.Equal(t, wantKeys, gotKeys)

	// The node sequences must describe the same constructor signatures.
	ctors := func(s *Scope) []reflect.Type {
		out := make([]reflect.Type, 0, len(s.gh.nodes))
		for _, gn := range s.gh.nodes {
			if n, ok := gn.Wrapped.(*constructorNode); ok {
				out = append(out, n.ctype)
			}
		}
		return out
	}
	assert.Equal(t, ctors(clean), ctors(failed))
}
