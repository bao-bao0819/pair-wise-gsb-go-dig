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

package dig_test

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/dig"
	"go.uber.org/dig/internal/digtest"
)

// batchDepA/B/C are plain types used across the batch dependency tests.
type batchDepA struct{ tag string }
type batchDepB struct{ a *batchDepA }
type batchDepC struct{ b *batchDepB }

func TestProvideBatchRegularDependencies(t *testing.T) {
	t.Parallel()

	c := digtest.New(t)
	require.NoError(t, c.ProvideBatch(
		dig.ProvideSpec{Constructor: func() *batchDepA { return &batchDepA{tag: "a"} }},
		dig.ProvideSpec{Constructor: func(a *batchDepA) *batchDepB { return &batchDepB{a: a} }},
		dig.ProvideSpec{Constructor: func(b *batchDepB) *batchDepC { return &batchDepC{b: b} }},
	))

	c.RequireInvoke(func(cc *batchDepC) {
		assert.Equal(t, "a", cc.b.a.tag)
	})
}

func TestProvideBatchNamedValues(t *testing.T) {
	t.Parallel()

	type namedParam struct {
		dig.In

		RO string `name:"ro"`
		RW string `name:"rw"`
	}

	c := digtest.New(t)
	require.NoError(t, c.ProvideBatch(
		dig.ProvideSpec{
			Constructor: func() string { return "read-only" },
			Options:     []dig.ProvideOption{dig.Name("ro")},
		},
		dig.ProvideSpec{
			Constructor: func() string { return "read-write" },
			Options:     []dig.ProvideOption{dig.Name("rw")},
		},
	))

	c.RequireInvoke(func(p namedParam) {
		assert.Equal(t, "read-only", p.RO)
		assert.Equal(t, "read-write", p.RW)
	})
}

func TestProvideBatchValueGroups(t *testing.T) {
	t.Parallel()

	type groupResult struct {
		dig.Out

		Value string `group:"things"`
	}
	type groupParam struct {
		dig.In

		Values []string `group:"things"`
	}

	c := digtest.New(t)
	require.NoError(t, c.ProvideBatch(
		dig.ProvideSpec{Constructor: func() groupResult { return groupResult{Value: "a"} }},
		dig.ProvideSpec{Constructor: func() groupResult { return groupResult{Value: "b"} }},
		dig.ProvideSpec{
			Constructor: func() string { return "c" },
			Options:     []dig.ProvideOption{dig.Group("things")},
		},
	))

	c.RequireInvoke(func(p groupParam) {
		assert.ElementsMatch(t, []string{"a", "b", "c"}, p.Values)
	})
}

type batchAsReader struct{ data string }

func (r *batchAsReader) Read(p []byte) (int, error) {
	return copy(p, r.data), io.EOF
}

func TestProvideBatchAsPropagation(t *testing.T) {
	t.Parallel()

	type holder struct {
		reader io.Reader
	}

	c := digtest.New(t)
	require.NoError(t, c.ProvideBatch(
		dig.ProvideSpec{
			Constructor: func() *batchAsReader { return &batchAsReader{data: "hello"} },
			Options:     []dig.ProvideOption{dig.As(new(io.Reader))},
		},
		dig.ProvideSpec{Constructor: func(r io.Reader) holder { return holder{reader: r} }},
	))

	c.RequireInvoke(func(h holder) {
		buf := make([]byte, 5)
		n, _ := h.reader.Read(buf)
		assert.Equal(t, "hello", string(buf[:n]))
	})

	// The concrete type is hidden when As is used.
	assert.Error(t, c.Invoke(func(r *batchAsReader) {}))
}

func TestProvideBatchExportPropagation(t *testing.T) {
	t.Parallel()

	type expA struct{}
	type expB struct{}

	root := digtest.New(t)
	child1 := root.Scope("child 1")
	child2 := root.Scope("child 2")
	grandchild := child1.Scope("grandchild")

	// One private constructor and one exported constructor, both in a
	// single batch submitted to child1.
	require.NoError(t, child1.ProvideBatch(
		dig.ProvideSpec{Constructor: func() expA { return expA{} }},
		dig.ProvideSpec{
			Constructor: func() expB { return expB{} },
			Options:     []dig.ProvideOption{dig.Export(true)},
		},
	))

	// Private value is only visible in child1 and its descendents.
	child1.RequireInvoke(func(expA) {})
	grandchild.RequireInvoke(func(expA) {})
	assert.Error(t, root.Invoke(func(expA) {}))
	assert.Error(t, child2.Invoke(func(expA) {}))

	// Exported value is visible everywhere, including the root and a
	// sibling scope that already existed at registration time.
	root.RequireInvoke(func(expB) {})
	child1.RequireInvoke(func(expB) {})
	child2.RequireInvoke(func(expB) {})
	grandchild.RequireInvoke(func(expB) {})
}

func TestProvideBatchForwardReference(t *testing.T) {
	t.Parallel()

	// The first constructor consumes a type produced by the last
	// constructor in the same batch.
	c := digtest.New(t)
	require.NoError(t, c.ProvideBatch(
		dig.ProvideSpec{Constructor: func(v int) string { return "answer" }},
		dig.ProvideSpec{Constructor: func() int { return 42 }},
	))

	c.RequireInvoke(func(s string) {
		assert.Equal(t, "answer", s)
	})
}

func TestProvideBatchDuplicateKeyRollsBack(t *testing.T) {
	t.Parallel()

	type dupA struct{}
	type dupB struct{}

	c := digtest.New(t)
	c.RequireProvide(func() dupA { return dupA{} })

	err := c.ProvideBatch(
		dig.ProvideSpec{Constructor: func() dupB { return dupB{} }},
		// dupA was already provided before the batch.
		dig.ProvideSpec{Constructor: func() dupA { return dupA{} }},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already provided")

	// Nothing from the batch is visible.
	assert.Error(t, c.Invoke(func(dupB) {}))
	// The original provider is untouched.
	c.RequireInvoke(func(dupA) {})
}

func TestProvideBatchDuplicateKeyWithinBatch(t *testing.T) {
	t.Parallel()

	type withinA struct{}
	type withinB struct{}

	c := digtest.New(t)
	err := c.ProvideBatch(
		dig.ProvideSpec{Constructor: func() withinB { return withinB{} }},
		dig.ProvideSpec{Constructor: func() withinA { return withinA{} }},
		dig.ProvideSpec{Constructor: func() withinA { return withinA{} }},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already provided")

	assert.Error(t, c.Invoke(func(withinA) {}))
	assert.Error(t, c.Invoke(func(withinB) {}))
}

func TestProvideBatchNamedConflictRollsBack(t *testing.T) {
	t.Parallel()

	type namedCheck struct {
		dig.In

		Svc string `name:"svc"`
	}

	c := digtest.New(t)
	c.RequireProvide(func() string { return "old" }, dig.Name("svc"))

	err := c.ProvideBatch(
		dig.ProvideSpec{Constructor: func() int { return 1 }},
		dig.ProvideSpec{
			Constructor: func() string { return "new" },
			Options:     []dig.ProvideOption{dig.Name("svc")},
		},
	)
	require.Error(t, err)

	assert.Error(t, c.Invoke(func(int) {}))
	c.RequireInvoke(func(p namedCheck) { assert.Equal(t, "old", p.Svc) })
}

func TestProvideBatchAsConflictRollsBack(t *testing.T) {
	t.Parallel()

	c := digtest.New(t)
	c.RequireProvide(func() io.Reader { return strings.NewReader("old") })

	err := c.ProvideBatch(
		dig.ProvideSpec{Constructor: func() int { return 7 }},
		dig.ProvideSpec{
			Constructor: func() *batchAsReader { return &batchAsReader{} },
			Options:     []dig.ProvideOption{dig.As(new(io.Reader))},
		},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already provided")

	assert.Error(t, c.Invoke(func(int) {}))
	c.RequireInvoke(func(r io.Reader) {})
}

func TestProvideBatchSelfCycleRollsBack(t *testing.T) {
	t.Parallel()

	type cycA struct{}
	type cycB struct{}

	c := digtest.New(t)
	err := c.ProvideBatch(
		dig.ProvideSpec{Constructor: func(b *cycB) *cycA { return &cycA{} }},
		dig.ProvideSpec{Constructor: func(a *cycA) *cycB { return &cycB{} }},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cycle")

	assert.Error(t, c.Invoke(func(*cycA) {}))
	assert.Error(t, c.Invoke(func(*cycB) {}))
}

// A constructor returning multiple results where only one output clashes
// must cause the entire batch to fail.
func TestProvideBatchMultiOutputOneConflict(t *testing.T) {
	t.Parallel()

	type multiA struct{}
	type multiB struct{}

	c := digtest.New(t)
	c.RequireProvide(func() multiB { return multiB{} })

	err := c.ProvideBatch(
		// multiA is fresh, but multiB already exists.
		dig.ProvideSpec{Constructor: func() (multiA, multiB) { return multiA{}, multiB{} }},
		dig.ProvideSpec{Constructor: func() int { return 1 }},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already provided")

	assert.Error(t, c.Invoke(func(multiA) {}))
	assert.Error(t, c.Invoke(func(int) {}))
	c.RequireInvoke(func(multiB) {})
}

func TestProvideBatchInvalidConstructorRollsBack(t *testing.T) {
	t.Parallel()

	c := digtest.New(t)
	err := c.ProvideBatch(
		dig.ProvideSpec{Constructor: func() int { return 1 }},
		dig.ProvideSpec{Constructor: "not a function"},
	)
	require.Error(t, err)

	assert.Error(t, c.Invoke(func(int) {}))
}

func TestProvideBatchEmptyIsNoOp(t *testing.T) {
	t.Parallel()

	c := digtest.New(t)
	assert.NoError(t, c.ProvideBatch())
	assert.NoError(t, c.Scope("child").ProvideBatch())
}
