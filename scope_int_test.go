// Copyright (c) 2022 Uber Technologies, Inc.
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
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScopeAncestorsAndStoresToRoot(t *testing.T) {
	c := New()
	s1 := c.Scope("child1")
	s2 := s1.Scope("child2")
	s3 := s2.Scope("child2")

	assert.Equal(t, []containerStore{s3, s2, s1, c.scope}, s3.storesToRoot())
	assert.Equal(t, []*Scope{s3, s2, s1, c.scope}, s3.ancestors())
}

func TestDecorationChainRegistrationsAreOrdered(t *testing.T) {
	type A struct{ v int }

	s := New().scope

	require.NoError(t, s.Provide(func() *A { return &A{} }))

	chain := s.DecorationChain("a")
	require.NoError(t, chain.Decorate("one", func(a *A) *A { return a }))
	require.NoError(t, chain.Decorate("two", func(a *A) *A { return a }))
	require.NoError(t, chain.Decorate("three", func(a *A) *A { return a }))

	nodes := s.getValueDecorators("", reflect.TypeOf(&A{}))
	require.Len(t, nodes, 3)

	assert.Equal(t, "a", nodes[0].chainName)
	assert.Equal(t, 1, nodes[0].chainStage)
	assert.Equal(t, "one", nodes[0].chainStageLabel)
	assert.Equal(t, 2, nodes[1].chainStage)
	assert.Equal(t, "two", nodes[1].chainStageLabel)
	assert.Equal(t, 3, nodes[2].chainStage)
	assert.Equal(t, "three", nodes[2].chainStageLabel)

	assert.Contains(t, nodes[0].stageIdentity(), `stage "one" (#1) of decoration chain "a"`)
	assert.Contains(t, nodes[1].stageIdentity(), `stage "two" (#2) of decoration chain "a"`)

	// A different chain appends another binding for the same key in
	// declaration order.
	require.NoError(t, s.DecorationChain("b").Decorate("only", func(a *A) *A { return a }))
	nodes = s.getValueDecorators("", reflect.TypeOf(&A{}))
	require.Len(t, nodes, 4)
	assert.Equal(t, "b", nodes[3].chainName)

	// Legacy Decorate still conflicts with any existing binding.
	err := s.Decorate(func(a *A) *A { return a })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "*dig.A already decorated")
}

func TestDecorationChainNodeOutputsAreIsolated(t *testing.T) {
	type A struct{ v int }

	parent := New().scope
	require.NoError(t, parent.Provide(func() *A { return &A{v: 0} }))
	require.NoError(t, parent.DecorationChain("a").Decorate("p", func(a *A) *A {
		return &A{v: a.v + 1}
	}))

	child := parent.Scope("child")
	require.NoError(t, child.DecorationChain("a").Decorate("c", func(a *A) *A {
		return &A{v: a.v + 100}
	}))

	var got *A
	require.NoError(t, child.Invoke(func(a *A) { got = a }))
	assert.Equal(t, 101, got.v)

	// Each scope's chain has its own node with its own cached output, and
	// the child node's intermediate output does not leak into the parent.
	pNodes := parent.getValueDecorators("", reflect.TypeOf(&A{}))
	cNodes := child.getValueDecorators("", reflect.TypeOf(&A{}))
	require.Len(t, pNodes, 1)
	require.Len(t, cNodes, 1)

	k := key{t: reflect.TypeOf(&A{})}
	pv, ok := parent.getNodeDecoratedValue(pNodes[0], k)
	require.True(t, ok)
	cv, ok := child.getNodeDecoratedValue(cNodes[0], k)
	require.True(t, ok)

	assert.Equal(t, 1, pv.Interface().(*A).v)
	assert.Equal(t, 101, cv.Interface().(*A).v)

	var parentGot *A
	require.NoError(t, parent.Invoke(func(a *A) { parentGot = a }))
	assert.Equal(t, 1, parentGot.v)
}
