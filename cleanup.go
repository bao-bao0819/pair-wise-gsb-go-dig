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
	"errors"
	"fmt"

	"go.uber.org/dig/internal/digreflect"
)

// CleanupCallback is a function registered with a provided constructor using
// [WithCleanupCallback]. It is called when the [Scope] that owns the
// constructor is Closed, but only if the constructor was actually called and
// completed successfully.
//
// A CleanupCallback is called at most once, regardless of how many values
// the constructor produced.
type CleanupCallback func() error

// WithCleanupCallback returns a [ProvideOption] which has Dig call the
// passed in [CleanupCallback] when the [Scope] owning the constructor is
// Closed.
//
// The callback is registered only after the constructor has run
// successfully: constructors that were never called, or that failed, do
// not have their cleanup callbacks run. Dig does not infer cleanup behavior
// from the types a constructor returns (e.g. io.Closer); cleanup happens
// only through callbacks explicitly attached with this option.
//
// For example, the following closes the connection when the container is
// closed:
//
//	c := dig.New()
//	var conn *Connection
//	c.Provide(func() *Connection {
//		conn = newConnection()
//		return conn
//	}, dig.WithCleanupCallback(conn.Close))
//
// See [Scope.Close] for the order in which cleanup callbacks run.
func WithCleanupCallback(callback CleanupCallback) ProvideOption {
	return withCleanupCallbackOption{callback: callback}
}

type withCleanupCallbackOption struct {
	callback CleanupCallback
}

var _ ProvideOption = withCleanupCallbackOption{}

func (o withCleanupCallbackOption) String() string {
	return fmt.Sprintf("WithCleanupCallback(%p)", o.callback)
}

func (o withCleanupCallbackOption) applyProvideOption(po *provideOptions) {
	po.CleanupCallback = o.callback
}

// cleanupEntry is a cleanup callback registered with a Scope by a
// successfully-run constructor.
type cleanupEntry struct {
	// location of the constructor that registered this cleanup.
	location *digreflect.Func

	// callback is the user-supplied cleanup function.
	callback CleanupCallback
}

// addCleanup records a cleanup callback to be run when this Scope is closed.
// Callbacks are appended in the order constructors successfully complete.
func (s *Scope) addCleanup(location *digreflect.Func, callback CleanupCallback) {
	s.cleanups = append(s.cleanups, cleanupEntry{
		location: location,
		callback: callback,
	})
}

// Close shuts down the Container, running cleanup callbacks registered with
// [WithCleanupCallback]. See [Scope.Close] for details.
func (c *Container) Close() error {
	return c.scope.Close()
}

// Close shuts down the Scope and releases resources created by constructors
// that were provided to it.
//
// Close first closes any child Scopes that are still open, in reverse order
// of their creation. It then runs this Scope's own cleanup callbacks in
// reverse order of construction: values that were built later (and may
// depend on earlier values) are cleaned up before the values they were
// built from. Values inherited from a parent Scope are owned by that parent
// and are not released by closing this Scope.
//
// A cleanup callback runs only if its constructor was actually called and
// completed successfully, and at most once per constructor no matter how
// many values the constructor produced.
//
// A failing cleanup callback does not stop the remaining callbacks from
// running; Close returns all callback errors joined together. Once a Scope
// is closed, it may no longer be used to resolve new values: subsequent
// Invoke calls fail. Calling Close again is a no-op that reports the same
// result as the first call.
func (s *Scope) Close() error {
	if s.closed {
		return s.closeErr
	}
	s.closed = true

	var errs []error

	// Close still-open child scopes first, in reverse order of creation
	// so that the shutdown order is stable.
	for i := len(s.childScopes) - 1; i >= 0; i-- {
		child := s.childScopes[i]
		if child.closed {
			continue
		}
		if err := child.Close(); err != nil {
			errs = append(errs, err)
		}
	}

	// Run this Scope's own cleanups in reverse order of construction:
	// dependents are cleaned up before the values they depend on, and
	// independent values are cleaned up in a stable, deterministic order.
	for i := len(s.cleanups) - 1; i >= 0; i-- {
		c := s.cleanups[i]
		if err := c.callback(); err != nil {
			errs = append(errs, fmt.Errorf("cleanup %v: %w", c.location, err))
		}
	}

	s.closeErr = errors.Join(errs...)
	return s.closeErr
}

// checkClosed reports an error if this Scope or any of its ancestors has
// been closed.
func (s *Scope) checkClosed() error {
	for sc := s; sc != nil; sc = sc.parentScope {
		if sc.closed {
			if sc.name == "" {
				return newErrInvalidInput("cannot use a closed container", nil)
			}
			return newErrInvalidInput(
				fmt.Sprintf("cannot use closed scope %q", sc.name), nil)
		}
	}
	return nil
}
