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

import "errors"

// CleanupCallback is a function registered with a provided constructor
// using [WithProviderCleanup]. It is called when the [Scope] that owns
// the constructor is closed, and only if the constructor ran successfully
// at least once. It may return an error to report cleanup failure.
type CleanupCallback func() error

// WithProviderCleanup returns a [ProvideOption] which registers a
// [CleanupCallback] with the corresponding constructor. The callback runs
// when the [Scope] that owns the constructor is closed with [Scope.Close].
//
// Dig only tracks instances that were actually created: the callback runs
// at most once, and only if the constructor completed successfully.
// A constructor that was never called, or that failed, does not have its
// cleanup run. A constructor that produces multiple outputs registers its
// cleanup exactly once.
//
// Dig does not implicitly manage values that happen to implement
// io.Closer; cleanup is always explicit.
//
// For example, the following closes the connection when the container
// is closed:
//
//	c := dig.New()
//	c.Provide(newConnection, dig.WithProviderCleanup(func() error {
//		return conn.Close()
//	}))
//	defer c.Close()
func WithProviderCleanup(cleanup CleanupCallback) ProvideOption {
	return withCleanupOption{cleanup: cleanup}
}

type withCleanupOption struct {
	cleanup CleanupCallback
}

var _ ProvideOption = withCleanupOption{}

func (o withCleanupOption) applyProvideOption(po *provideOptions) {
	po.Cleanup = o.cleanup
}

// addCleanup records a constructor node whose cleanup callback must run
// when this Scope is closed. Nodes are recorded in the order their
// constructors successfully ran.
func (s *Scope) addCleanup(n *constructorNode) {
	s.cleanups = append(s.cleanups, n)
}

// Close shuts down the Container, running cleanup callbacks registered
// with [WithProviderCleanup] for every constructor that ran successfully.
// See [Scope.Close] for the exact semantics.
func (c *Container) Close() error {
	return c.scope.Close()
}

// Close shuts down the Scope, running cleanup callbacks registered with
// [WithProviderCleanup].
//
// Close first closes any child Scopes that are still open, then runs the
// cleanup callbacks of this Scope's own constructors in the reverse order
// in which they were created, so that consumers of a resource are cleaned
// up before the resource itself. Instances inherited from a parent Scope
// are owned by the parent: closing this Scope does not clean them up.
//
// A failing cleanup callback does not stop the remaining callbacks from
// running; Close returns all cleanup errors joined together. Close is
// idempotent: subsequent calls return the same result without running
// any callbacks again. Once closed, the Scope no longer resolves new
// values: calls to Invoke will fail.
func (s *Scope) Close() error {
	if s.closed {
		return s.closeErr
	}
	s.closed = true

	var errs []error

	// Close child scopes that are still alive before releasing
	// this Scope's own resources, since children may depend on them.
	for _, child := range s.childScopes {
		if child.closed {
			continue
		}
		if err := child.Close(); err != nil {
			errs = append(errs, err)
		}
	}

	// Run cleanups in reverse order of creation so that dependents are
	// cleaned up before the resources they depend on. Independent nodes
	// are cleaned up in a deterministic, stable order.
	for i := len(s.cleanups) - 1; i >= 0; i-- {
		n := s.cleanups[i]
		if err := n.cleanup(); err != nil {
			errs = append(errs, errCleanupFailed{
				Func:   n.location,
				Reason: err,
			})
		}
	}

	s.closeErr = errors.Join(errs...)
	return s.closeErr
}
