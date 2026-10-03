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
	"fmt"
	"reflect"

	"go.uber.org/dig/internal/digreflect"
	"go.uber.org/dig/internal/graph"
)

// BatchProvider pairs a constructor with the ProvideOptions that apply to
// it when it is submitted as part of a batch via ProvideBatch.
type BatchProvider struct {
	// Constructor is a function that accepts zero or more parameters and
	// returns one or more results. It follows the same rules as the
	// constructor argument of Provide.
	//
	// The constructor is never called during batch registration; it is only
	// inspected and invoked later, at most once, when one of its results is
	// requested via Invoke.
	Constructor interface{}

	// Options holds the ProvideOptions for this constructor, such as
	// Name, Group, As, Export and FillProvideInfo.
	Options []ProvideOption
}

// ProvideBatch teaches the container how to build values using a group of
// constructors, each with its own options.
//
// Unlike calling Provide once per constructor, the whole batch is applied
// atomically: either every constructor in the batch is registered, or none
// of them are. If any constructor is invalid, produces a type that is
// already provided (unless it feeds a value group), or if the combined
// dependency graph contains a cycle, ProvideBatch returns an error and the
// Container is left exactly as it was before the call. No providers, graph
// nodes or scope exports from the failed batch remain.
//
// Constructors earlier in the batch may depend on values produced by
// constructors later in the batch; the order inside the batch does not
// matter as long as the graph formed after committing the whole batch is
// acyclic. The cycle check always runs when the batch commits, even for
// Containers created with DeferAcyclicVerification.
//
// None of the constructors are called during registration.
func (c *Container) ProvideBatch(providers ...BatchProvider) error {
	return c.scope.ProvideBatch(providers...)
}

// ProvideBatch teaches the Scope how to build values using a group of
// constructors, each with its own options.
//
// The batch is applied atomically: either every constructor in the batch is
// registered, or none of them are. A failed batch leaves the Scope and all
// of its descendent Scopes unchanged.
//
// Constructors earlier in the batch may depend on values produced by
// constructors later in the batch, but the dependency graph visible after
// the batch commits must be acyclic. The check always runs when the batch
// commits, even for Scopes that defer acyclic verification otherwise.
//
// Constructors provided with the Export option are registered on the root
// Scope; the rest are registered on this Scope. None of the constructors
// are called while the batch is being registered.
func (s *Scope) ProvideBatch(providers ...BatchProvider) (err error) {
	if len(providers) == 0 {
		return nil
	}

	// Validate every constructor and its options before mutating anything.
	// This mirrors the upfront checks done by Provide.
	type batchEntry struct {
		ctor    interface{}
		options provideOptions
		errFunc *digreflect.Func
	}
	entries := make([]batchEntry, len(providers))
	for i, p := range providers {
		ctype := reflect.TypeOf(p.Constructor)
		if ctype == nil {
			return newErrInvalidInput("can't provide an untyped nil", nil)
		}
		if ctype.Kind() != reflect.Func {
			return newErrInvalidInput(
				fmt.Sprintf("must provide constructor function, got %v (type %v)", p.Constructor, ctype), nil)
		}

		var options provideOptions
		for _, o := range p.Options {
			o.applyProvideOption(&options)
		}
		if err := options.Validate(); err != nil {
			return err
		}

		errFunc := options.Location
		if errFunc == nil {
			errFunc = digreflect.InspectFunc(p.Constructor)
		}
		entries[i] = batchEntry{
			ctor:    p.Constructor,
			options: options,
			errFunc: errFunc,
		}
	}

	// Exported constructors land on the root Scope and propagate to every
	// Scope; the rest land on this Scope and propagate to its descendents.
	// If the batch contains any exported constructor, snapshot the whole
	// tree; otherwise snapshotting this Scope's subtree is sufficient.
	anyExported := false
	for _, e := range entries {
		if e.options.Exported {
			anyExported = true
			break
		}
	}
	topScope := s
	if anyExported {
		topScope = s.rootScope()
	}
	allScopes := topScope.appendSubscopes(nil)

	// scopeState captures the state of a Scope that may be modified while
	// the batch is being staged so it can be restored on failure.
	type providerBackup struct {
		nodes []*constructorNode
		ok    bool
	}
	type scopeState struct {
		sc       *Scope
		verified bool
		nodesLen int
		// providers holds the previous providers entry for every key that
		// the batch wrote to on this Scope.
		providers map[key]providerBackup
	}
	states := make(map[*Scope]*scopeState, len(allScopes))
	stateFor := func(sc *Scope) *scopeState {
		st := states[sc]
		if st == nil {
			st = &scopeState{
				sc:        sc,
				verified:  sc.isVerifiedAcyclic,
				nodesLen:  len(sc.nodes),
				providers: make(map[key]providerBackup),
			}
			states[sc] = st
		}
		return st
	}

	committed := false
	defer func() {
		if committed || err == nil {
			return
		}

		for _, sc := range allScopes {
			sc.gh.Rollback()
		}

		for _, st := range states {
			sc := st.sc
			for k, old := range st.providers {
				if !old.ok {
					delete(sc.providers, k)
				} else {
					sc.providers[k] = old.nodes
				}
			}
			sc.nodes = sc.nodes[:st.nodesLen]
			sc.isVerifiedAcyclic = st.verified
		}
	}()

	for _, sc := range allScopes {
		stateFor(sc)
		sc.gh.Snapshot()
	}

	// stagedNode is a constructor that has passed validation and been
	// added to the graph/provider maps, but not yet committed to its
	// target Scope's node list.
	type stagedNode struct {
		node   *constructorNode
		target *Scope
		info   *ProvideInfo
	}
	staged := make([]stagedNode, 0, len(entries))

	for _, e := range entries {
		target := s
		origScope := s
		if e.options.Exported {
			target = s.rootScope()
		}

		n, buildErr := newConstructorNode(
			e.ctor,
			target,
			origScope,
			constructorOptions{
				ResultName:     e.options.Name,
				ResultGroup:    e.options.Group,
				ResultAs:       e.options.As,
				Location:       e.options.Location,
				Callback:       e.options.Callback,
				BeforeCallback: e.options.BeforeCallback,
			},
		)
		if buildErr != nil {
			return errProvide{Func: e.errFunc, Reason: buildErr}
		}

		keys, buildErr := target.findAndValidateResults(n.ResultList())
		if buildErr != nil {
			return errProvide{Func: e.errFunc, Reason: buildErr}
		}

		ctype := reflect.TypeOf(e.ctor)
		if len(keys) == 0 {
			return errProvide{
				Func: e.errFunc,
				Reason: newErrInvalidInput(
					fmt.Sprintf("%v must provide at least one non-error type", ctype), nil),
			}
		}

		st := stateFor(target)
		for k := range keys {
			if _, touched := st.providers[k]; !touched {
				old, ok := target.providers[k]
				st.providers[k] = providerBackup{nodes: old, ok: ok}
			}
			target.providers[k] = append(target.providers[k], n)
		}

		staged = append(staged, stagedNode{
			node:   n,
			target: target,
			info:   e.options.Info,
		})
	}

	// Every constructor is staged and resolvable against the others in the
	// batch. Verify the resulting graph on every affected Scope. This runs
	// unconditionally, including on containers created with
	// DeferAcyclicVerification, so a cyclic batch fails at registration.
	for _, sc := range allScopes {
		sc.isVerifiedAcyclic = false
		if ok, cycle := graph.IsAcyclic(sc.gh); !ok {
			return newErrInvalidInput(
				"this function introduces a cycle", sc.cycleDetectedError(cycle))
		}
		sc.isVerifiedAcyclic = true
	}

	// Commit: the graph nodes and provider entries are already in place;
	// record the nodes on their target Scopes and publish caller-requested
	// introspection info.
	for _, sn := range staged {
		sn.target.nodes = append(sn.target.nodes, sn.node)
		if info := sn.info; info != nil {
			fillNodeProvideInfo(info, sn.node)
		}
	}

	committed = true
	return nil
}

// fillNodeProvideInfo populates a ProvideInfo from a registered constructor
// node.
func fillNodeProvideInfo(info *ProvideInfo, n *constructorNode) {
	params := n.ParamList().DotParam()
	results := n.ResultList().DotResult()

	info.ID = ID(n.id)
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
