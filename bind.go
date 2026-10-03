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
	"sort"
	"strings"

	"go.uber.org/dig/internal/dot"
	"go.uber.org/dig/internal/graph"
)

// InterfaceBindingPlan is a plan to bind interfaces to concrete types
// already known to the container.
//
// A plan is created with [Container.PlanInterfaceBindings] or
// [Scope.PlanInterfaceBindings]. Planning inspects the signatures of the
// constructors registered so far -- it never runs them -- and selects, for
// each requested interface, the unique unnamed, ungrouped concrete output
// visible from the scope that is assignable to that interface.
//
// Nothing is installed until [InterfaceBindingPlan.Commit] is called, and
// Commit installs the whole plan atomically: either every binding is
// installed, or none of them are.
type InterfaceBindingPlan struct {
	scope     *Scope
	version   uint64
	bindings  []plannedBinding
	committed bool
}

// plannedBinding is a single interface-to-concrete-type binding.
type plannedBinding struct {
	iface    reflect.Type
	concrete reflect.Type

	// node is the constructor that produces the concrete type, and path is
	// the position of the concrete type in that constructor's results
	// (e.g. "[0]" or "[0].Foo" for a field of a result object).
	node *constructorNode
	path string
}

func (b plannedBinding) String() string {
	return fmt.Sprintf("%v as %v (output %v of %v)", b.concrete, b.iface, b.path, b.node.Location())
}

// PlanInterfaceBindings plans bindings from the given interfaces to concrete
// types produced by constructors already registered with the Container.
// See Scope.PlanInterfaceBindings for details.
func (c *Container) PlanInterfaceBindings(ifaces ...interface{}) (*InterfaceBindingPlan, error) {
	return c.scope.PlanInterfaceBindings(ifaces...)
}

// PlanInterfaceBindings plans bindings from the given interfaces to concrete
// types produced by constructors already registered with the Scope.
//
// Each argument must be a pointer to an interface, as with [As]. For every
// requested interface, the plan searches the outputs of all providers
// visible from this Scope for a concrete type assignable to the interface:
//
//   - only unnamed outputs that are not part of a value group are
//     considered;
//   - assignability follows Go method sets as-is: a value type does not
//     gain the methods of its pointer type, and no address is taken
//     implicitly;
//   - outputs of constructors in nearer Scopes shadow outputs of the same
//     type in ancestor Scopes, matching the container's resolution rules;
//   - an interface that already has an explicit provider (for example, one
//     registered with [As]) is left untouched: the explicit provider takes
//     precedence and no binding is planned for it.
//
// Planning only inspects signatures; constructors are never executed. If an
// interface has zero or more than one candidate, planning fails with a
// diagnostic listing the candidate constructors and output positions, and
// no plan is created. Ambiguities may always be resolved manually with
// [As] or an explicit constructor before planning again.
//
// The returned plan is a snapshot: any successful Provide, Decorate, or
// committed binding plan in this scope tree invalidates it, and
// [InterfaceBindingPlan.Commit] of an invalidated plan fails.
func (s *Scope) PlanInterfaceBindings(ifaces ...interface{}) (*InterfaceBindingPlan, error) {
	seen := make(map[reflect.Type]struct{})
	var bindings []plannedBinding
	for _, raw := range ifaces {
		t := reflect.TypeOf(raw)
		if t == nil {
			return nil, newErrInvalidInput(
				"invalid interface binding target nil: argument must be a pointer to an interface", nil)
		}
		if t.Kind() != reflect.Ptr || t.Elem().Kind() != reflect.Interface {
			return nil, newErrInvalidInput(fmt.Sprintf(
				"invalid interface binding target %v: argument must be a pointer to an interface", t), nil)
		}
		iface := t.Elem()
		if _, ok := seen[iface]; ok {
			continue
		}
		seen[iface] = struct{}{}

		// An explicit provider for the interface takes precedence over
		// any binding we could plan.
		if len(s.getAllValueProviders("", iface)) > 0 {
			continue
		}

		candidates := s.bindingCandidates(iface)
		switch len(candidates) {
		case 0:
			return nil, newErrInvalidInput(fmt.Sprintf(
				"cannot bind %v: no unnamed, ungrouped provider output visible from this scope is assignable to it", iface), nil)
		case 1:
			bindings = append(bindings, candidates[0])
		default:
			var msg strings.Builder
			fmt.Fprintf(&msg, "cannot bind %v: multiple provider outputs are assignable to it:", iface)
			for _, c := range candidates {
				fmt.Fprintf(&msg, "\n\t%v", c)
			}
			return nil, newErrInvalidInput(msg.String(), nil)
		}
	}
	return &InterfaceBindingPlan{
		scope:    s,
		version:  s.rootScope().registrations,
		bindings: bindings,
	}, nil
}

// bindingCandidates returns the bindings that could satisfy the given
// interface from the providers visible to this Scope. Outputs of the same
// type provided by a nearer Scope shadow those of ancestor Scopes.
func (s *Scope) bindingCandidates(iface reflect.Type) []plannedBinding {
	var candidates []plannedBinding
	shadowed := make(map[reflect.Type]struct{})
	for _, scope := range s.ancestors() {
		keys := make([]key, 0, len(scope.providers))
		for k := range scope.providers {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			return fmt.Sprint(keys[i].t) < fmt.Sprint(keys[j].t)
		})

		for _, k := range keys {
			// Named values and value groups never mix into
			// unnamed candidates.
			if k.name != "" || k.group != "" {
				continue
			}
			if _, ok := shadowed[k.t]; ok {
				continue
			}
			shadowed[k.t] = struct{}{}

			// Only concrete types are candidates, and only if
			// their own method set satisfies the interface:
			// no address is taken implicitly.
			if k.t.Kind() == reflect.Interface {
				continue
			}
			if !k.t.Implements(iface) {
				continue
			}
			for _, n := range scope.providers[k] {
				candidates = append(candidates, plannedBinding{
					iface:    iface,
					concrete: k.t,
					node:     n,
					path:     resultPathFor(n.ResultList(), k.t),
				})
			}
		}
	}
	return candidates
}

// resultPathFor returns the position of the unnamed result with the given
// type inside the result list, in the form "[0]" or "[0].Foo" for a field of
// a result object.
func resultPathFor(rl resultList, t reflect.Type) string {
	path, _ := findResultPath(rl, t, "")
	return path
}

func findResultPath(r result, t reflect.Type, prefix string) (string, bool) {
	switch res := r.(type) {
	case resultSingle:
		if res.Name == "" && res.Type == t {
			return prefix, true
		}
	case resultObject:
		for _, f := range res.Fields {
			path := f.FieldName
			if prefix != "" {
				path = prefix + "." + path
			}
			if got, ok := findResultPath(f.Result, t, path); ok {
				return got, true
			}
		}
	case resultList:
		for i, inner := range res.Results {
			path := fmt.Sprintf("[%d]", i)
			if prefix != "" {
				path = prefix + "." + path
			}
			if got, ok := findResultPath(inner, t, path); ok {
				return got, true
			}
		}
	}
	return "", false
}

// String returns a human-readable description of the planned bindings.
func (p *InterfaceBindingPlan) String() string {
	var msg strings.Builder
	msg.WriteString("interface binding plan: {")
	for _, b := range p.bindings {
		fmt.Fprintf(&msg, "\n\t%v", b)
	}
	msg.WriteString("\n}")
	return msg.String()
}

// Commit installs all bindings of the plan into the Scope the plan was
// created from.
//
// The installation is atomic: the bindings are validated together, and if
// any of them cannot be installed -- for example because the bindings would
// introduce a dependency cycle -- none of them are left behind. The
// diagnostic for a cycle lists the candidate constructors and output
// positions of the plan alongside the cycle itself.
//
// Commit fails if the plan was already committed, or if any Provide,
// Decorate, or other committed binding plan changed the container after
// the plan was created. Constructors are not run by Commit; the installed
// bindings take effect on the next Invoke.
func (p *InterfaceBindingPlan) Commit() error {
	if p.committed {
		return newErrInvalidInput("interface binding plan has already been committed", nil)
	}
	s := p.scope
	if s.rootScope().registrations != p.version {
		return newErrInvalidInput(
			"interface binding plan is no longer valid: the container changed after the plan was created", nil)
	}
	if err := s.installBindings(p.bindings); err != nil {
		return err
	}
	p.committed = true
	s.noteRegistration()
	return nil
}

// installBindings installs the given planned bindings into the Scope
// atomically: if any binding fails validation, all changes are rolled back.
func (s *Scope) installBindings(bindings []plannedBinding) (err error) {
	if len(bindings) == 0 {
		return nil
	}

	// For all scopes affected by this change, take a snapshot of the
	// current graph state before we start making changes to it as we may
	// need to undo them upon encountering errors.
	allScopes := s.appendSubscopes(nil)
	for _, sc := range allScopes {
		sc.gh.Snapshot()
	}

	oldProviders := make(map[key][]*constructorNode)
	defer func() {
		if err != nil {
			for k, ops := range oldProviders {
				s.providers[k] = ops
			}
			for _, sc := range allScopes {
				sc.gh.Rollback()
			}
		}
	}()

	var nodes []*constructorNode
	for _, b := range bindings {
		var n *constructorNode
		n, err = newBindingNode(s, b)
		if err != nil {
			return err
		}
		k := key{t: b.iface}
		if _, ok := oldProviders[k]; !ok {
			oldProviders[k] = s.providers[k]
		}
		s.providers[k] = append(s.providers[k], n)
		nodes = append(nodes, n)
	}

	for _, sc := range allScopes {
		sc.isVerifiedAcyclic = false
		if sc.deferAcyclicVerification {
			continue
		}
		if ok, cycle := graph.IsAcyclic(sc.gh); !ok {
			var msg strings.Builder
			msg.WriteString("interface binding plan introduces a dependency cycle:")
			for _, b := range bindings {
				fmt.Fprintf(&msg, "\n\t%v", b)
			}
			err = newErrInvalidInput(msg.String(), sc.cycleDetectedError(cycle))
			return err
		}
		sc.isVerifiedAcyclic = true
	}

	s.nodes = append(s.nodes, nodes...)
	return nil
}

// newBindingNode builds a constructor node for a single planned binding.
// The synthesized constructor adapts the concrete type to the interface; it
// borrows the location of the constructor that produces the concrete type
// so that diagnostics point at user code.
func newBindingNode(s *Scope, b plannedBinding) (*constructorNode, error) {
	ctype := reflect.FuncOf(
		[]reflect.Type{b.concrete},
		[]reflect.Type{b.iface},
		false, /* variadic */
	)
	ctor := reflect.MakeFunc(ctype, func(args []reflect.Value) []reflect.Value {
		return args
	}).Interface()

	n, err := newConstructorNode(ctor, s, s, constructorOptions{
		Location: b.node.Location(),
	})
	if err != nil {
		return nil, err
	}
	// Functions created by reflect.MakeFunc do not have unique code
	// pointers, so identify the node by its own address instead.
	n.id = dot.CtorID(reflect.ValueOf(n).Pointer())
	return n, nil
}
