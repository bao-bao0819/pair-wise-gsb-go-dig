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

package dig_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/dig"
	"go.uber.org/dig/internal/digtest"
)

// A counting constructor records how many times it ran. Constructors must
// never run during Provide/ProvideBatch registration.
type countingCtor struct {
	calls int
}

func (cc *countingCtor) newA() *batchDepA {
	cc.calls++
	return &batchDepA{tag: "counted"}
}

func TestProvideBatchZeroExecution(t *testing.T) {
	t.Parallel()

	c := digtest.New(t)
	aCtor := &countingCtor{}
	bCtor := &countingCtor{}

	// Successful registration does not execute constructors.
	require.NoError(t, c.ProvideBatch(
		dig.ProvideSpec{Constructor: aCtor.newA},
		dig.ProvideSpec{Constructor: func(a *batchDepA) *batchDepB { return &batchDepB{a: a} }},
	))
	assert.Equal(t, 0, aCtor.calls)

	// A failed batch must also execute nothing.
	err := c.ProvideBatch(
		dig.ProvideSpec{Constructor: bCtor.newA},
		// *batchDepA already provided above, so the batch fails.
		dig.ProvideSpec{Constructor: func() *batchDepA { return &batchDepA{} }},
	)
	require.Error(t, err)
	assert.Equal(t, 0, bCtor.calls)
	assert.Equal(t, 0, aCtor.calls)
}

// ProvideBatch must perform the acyclic check even on containers built with
// DeferAcyclicVerification, where a single Provide would defer the check.
func TestProvideBatchDeferredAcyclicVerification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts []dig.Option
	}{
		{"deferred", []dig.Option{dig.DeferAcyclicVerification()}},
		{"deferred dry run", []dig.Option{dig.DeferAcyclicVerification(), dig.DryRun(true)}},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			type dA struct{}
			type dB struct{}

			c := digtest.New(t, tt.opts...)

			// A single deferred Provide does not report the cycle yet.
			c.RequireProvide(func(*dB) *dA { return &dA{} })

			// ...but the batch check must catch the cycle immediately.
			err := c.ProvideBatch(
				dig.ProvideSpec{Constructor: func(*dA) *dB { return &dB{} }},
			)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "cycle")

			// Rolled back: dB is unavailable.
			assert.Error(t, c.Invoke(func(*dB) {}))
		})
	}
}

// A cycle that only materializes across a parent and a child Scope must be
// caught and rolled back in all affected Scopes.
func TestProvideBatchCrossScopeCycle(t *testing.T) {
	t.Parallel()

	// root and child1 see:  A <- B      C
	//                              ^----|
	// child2 (which has B) sees the full cycle A <- B <- C <- A.
	type crA struct{}
	type crB struct{}
	type crC struct{}

	newA := func(*crC) *crA { return &crA{} }
	newB := func(*crA) *crB { return &crB{} }
	newC := func(*crB) *crC { return &crC{} }

	root := digtest.New(t)
	root.RequireProvide(newA)

	child1 := root.Scope("child 1")
	child2 := root.Scope("child 2")
	child2.RequireProvide(newB)

	// Export C from child1 so it lands on the root and propagates to
	// child2, where it closes the cycle.
	err := child1.ProvideBatch(
		dig.ProvideSpec{
			Constructor: newC,
			Options:     []dig.ProvideOption{dig.Export(true)},
		},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cycle")
	assert.Contains(t, err.Error(), `[scope "child 2"]`)

	// C was never committed anywhere.
	assert.Error(t, root.Invoke(func(*crC) {}))
	assert.Error(t, child1.Invoke(func(*crC) {}))
	assert.Error(t, child2.Invoke(func(*crC) {}))
}

// After a failing batch is corrected and resubmitted, the container must
// behave identically to one that only ever received the corrected batch.
func TestProvideBatchRetryAfterFailure(t *testing.T) {
	t.Parallel()

	build := func(seedFailure bool) *digtest.Container {
		c := digtest.New(t)
		c.RequireProvide(func() int { return 1 })

		if seedFailure {
			// Conflicts on the already-provided int, so this fails.
			_ = c.ProvideBatch(
				dig.ProvideSpec{Constructor: func() string { return "x" }},
				dig.ProvideSpec{Constructor: func() int { return 2 }},
			)
		}

		// Corrected batch: only the new types are provided.
		require.NoError(t, c.ProvideBatch(
			dig.ProvideSpec{Constructor: func() string { return "ok" }},
			dig.ProvideSpec{Constructor: func(i int, s string) float64 {
				assert.Equal(t, 1, i)
				assert.Equal(t, "ok", s)
				return 3.14
			}},
		))
		return c
	}

	failed := build(true)
	clean := build(false)

	// Both containers resolve the same graph with the same values.
	for _, c := range []*digtest.Container{failed, clean} {
		c.RequireInvoke(func(i int, s string, f float64) {
			assert.Equal(t, 1, i)
			assert.Equal(t, "ok", s)
			assert.Equal(t, 3.14, f)
		})
	}
}
