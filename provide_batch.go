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

package dig

import (
	"fmt"
	"reflect"

	"go.uber.org/dig/internal/digreflect"
	"go.uber.org/dig/internal/graph"
)

// ProvideSpec is a single entry in a batch handed to [Scope.ProvideBatch].
// It pairs a constructor with the ProvideOptions that apply to it, such as
// [Name], [Group], [As] or [Export].
type ProvideSpec struct {
	// Constructor taught to the container in this batch entry.
	// It follows the same rules as the argument of [Scope.Provide].
	Constructor interface{}

	// Options applied to Constructor, e.g. [Name], [Group], [As], [Export].
	Options []ProvideOption
}

// parsedProvideSpec is a ProvideSpec whose options have already been
// validated and applied.
type parsedProvideSpec struct {
	ctor interface{}
	opts provideOptions
}

// ProvideBatch registers a group of constructors atomically. It accepts a
// list of [ProvideSpec] values, each pairing a constructor with its own
// options.
//
// Either every constructor in the batch becomes visible to the container at
// once, or none of them do. If any constructor is invalid, produces a type
// that was already provided, or the combined constructors introduce a
// dependency cycle, none of the batch's constructors are registered and the
// container is left exactly as it was before the call.
//
// Constructors appearing earlier in the batch may depend on values produced
// by constructors appearing later in it; the whole batch is treated as a
// single graph. The resulting graph must still be acyclic. This check always
// runs for ProvideBatch, even on containers built with
// [DeferAcyclicVerification].
//
// As with [Scope.Provide], constructors are not executed during
// registration. Only the graph and metadata of the affected Scopes are
// modified, and all such modifications are rolled back on failure.
//
// An empty batch is a no-op.
func (c *Container) ProvideBatch(specs ...ProvideSpec) error {
	return c.scope.ProvideBatch(specs...)
}

// ProvideBatch behaves like [Container.ProvideBatch] on a Scope. See that
// method for the atomicity and validation guarantees.
//
// A spec using the [Export] option is registered on the root Scope and
// propagated to every Scope; other specs stay private to this Scope and are
// propagated to its descendents only.
func (s *Scope) ProvideBatch(specs ...ProvideSpec) error {
	if len(specs) == 0 {
		return nil
	}

	parsed := make([]parsedProvideSpec, len(specs))
	for i, spec := range specs {
		ctype := reflect.TypeOf(spec.Constructor)
		if ctype == nil {
			return newErrInvalidInput("can't provide an untyped nil", nil)
		}
		if ctype.Kind() != reflect.Func {
			return newErrInvalidInput(
				fmt.Sprintf("must provide constructor function, got %v (type %v)", spec.Constructor, ctype), nil)
		}

		var options provideOptions
		for _, o := range spec.Options {
			o.applyProvideOption(&options)
		}
		if err := options.Validate(); err != nil {
			return wrapProvideError(spec.Constructor, options, err)
		}
		parsed[i] = parsedProvideSpec{ctor: spec.Constructor, opts: options}
	}

	return s.provideBatch(parsed)
}

func wrapProvideError(ctor interface{}, options provideOptions, err error) error {
	errFunc := options.Location
	if errFunc == nil {
		errFunc = digreflect.InspectFunc(ctor)
	}
	return errProvide{Func: errFunc, Reason: err}
}

// scopeSnapshot captures the mutable state of a Scope that ProvideBatch may
// modify, so it can be restored on failure.
type scopeSnapshot struct {
	// Previous provider slices for keys the batch appended to.
	// A nil entry means the key did not exist before the batch.
	providers map[key][]*constructorNode

	// Length of Scope.nodes before the batch.
	nodeCount int

	// Previous acyclic-verification state.
	verified bool
}

// provideBatch commits the given already-validated specs as a single atomic
// transaction. On any error every modification made to the affected Scopes
// is rolled back and the container is left untouched.
func (s *Scope) provideBatch(specs []parsedProvideSpec) (err error) {
	// Determine the target Scope for every spec: exported constructors are
	// hoisted to the root Scope, all others land on this Scope.
	targets := make([]*Scope, len(specs))
	affectedSet := make(map[*Scope]struct{})
	var affected []*Scope
	addAffected := func(target *Scope) {
		for _, sc := range target.appendSubscopes(nil) {
			if _, ok := affectedSet[sc]; !ok {
				affectedSet[sc] = struct{}{}
				affected = append(affected, sc)
			}
		}
	}
	for i, sp := range specs {
		target := s
		if sp.opts.Exported {
			target = s.rootScope()
		}
		targets[i] = target
		addAffected(target)
	}

	// Snapshot the graph and mutable metadata of every affected Scope
	// before any change is made.
	snapshots := make(map[*Scope]*scopeSnapshot, len(affected))
	for _, sc := range affected {
		sc.gh.Snapshot()
		snapshots[sc] = &scopeSnapshot{
			providers: make(map[key][]*constructorNode),
			nodeCount: len(sc.nodes),
			verified:  sc.isVerifiedAcyclic,
		}
	}

	rollback := func() {
		for _, sc := range affected {
			snap := snapshots[sc]
			for k, old := range snap.providers {
				if old == nil {
					delete(sc.providers, k)
				} else {
					sc.providers[k] = old
				}
			}
			sc.nodes = sc.nodes[:snap.nodeCount]
			sc.isVerifiedAcyclic = snap.verified
			sc.gh.Rollback()
		}
	}

	type registeredNode struct {
		node   *constructorNode
		target *Scope
		opts   provideOptions
	}
	registered := make([]registeredNode, 0, len(specs))

	// Build and tentatively register every constructor.
	for i, sp := range specs {
		target := targets[i]

		n, err := newConstructorNode(
			sp.ctor,
			target,
			s,
			constructorOptions{
				ResultName:     sp.opts.Name,
				ResultGroup:    sp.opts.Group,
				ResultAs:       sp.opts.As,
				Location:       sp.opts.Location,
				Callback:       sp.opts.Callback,
				BeforeCallback: sp.opts.BeforeCallback,
			},
		)
		if err != nil {
			rollback()
			return wrapProvideError(sp.ctor, sp.opts, err)
		}

		keys, err := target.findAndValidateResults(n.ResultList())
		if err != nil {
			rollback()
			return wrapProvideError(sp.ctor, sp.opts, err)
		}

		ctype := reflect.TypeOf(sp.ctor)
		if len(keys) == 0 {
			rollback()
			return wrapProvideError(sp.ctor, sp.opts, newErrInvalidInput(
				fmt.Sprintf("%v must provide at least one non-error type", ctype), nil))
		}

		snap := snapshots[target]
		for k := range keys {
			if _, saved := snap.providers[k]; !saved {
				snap.providers[k] = target.providers[k]
			}
			target.providers[k] = append(target.providers[k], n)
		}
		target.nodes = append(target.nodes, n)

		registered = append(registered, registeredNode{
			node:   n,
			target: target,
			opts:   sp.opts,
		})
	}

	// Verify the combined graph of every affected Scope. Earlier batch
	// entries are now allowed to consume values produced by later ones.
	// This runs even when acyclic verification is otherwise deferred.
	var cycleErr error
	for _, sc := range affected {
		sc.isVerifiedAcyclic = false
		if ok, cycle := graph.IsAcyclic(sc.gh); !ok {
			// Build the error while the tentative graph is still
			// available, then roll back; cycleDetectedError looks up
			// graph nodes by index.
			cycleErr = newErrInvalidInput(
				"this function introduces a cycle", sc.cycleDetectedError(cycle))
			break
		}
		sc.isVerifiedAcyclic = true
	}
	if cycleErr != nil {
		rollback()
		return cycleErr
	}

	// Commit: discard graph snapshots and record introspection info.
	for _, sc := range affected {
		sc.gh.Commit()
	}
	for _, rn := range registered {
		info := rn.opts.Info
		if info == nil {
			continue
		}
		params := rn.node.ParamList().DotParam()
		results := rn.node.ResultList().DotResult()

		info.ID = ID(rn.node.id)
		info.Inputs = make([]*Input, len(params))
		info.Outputs = make([]*Output, len(results))

		for i, param := range params {
			info.Inputs[i] = &Input{
				t:        param.Type,
				optional: param.Optional,
				name:     param.Name,
				group:    param.Group,
			}
		}
		for i, res := range results {
			info.Outputs[i] = &Output{
				t:     res.Type,
				name:  res.Name,
				group: res.Group,
			}
		}
	}

	return nil
}
