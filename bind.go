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
	"io"
	"reflect"
	"sort"
	"strings"

	"go.uber.org/dig/internal/digreflect"
	"go.uber.org/dig/internal/graph"
)

// BindingPlan is a plan to bind interfaces to concrete types already
// available in the container. It is created with
// [Container.PlanInterfaceBindings] or [Scope.PlanInterfaceBindings] and
// installed atomically with [BindingPlan.Commit].
//
// A plan is a snapshot of the providers visible at the time it was created.
// If any constructor is registered with a Scope visible to the plan after
// the plan was created, the plan becomes stale and Commit will refuse to
// install it.
type BindingPlan struct {
	scope     *Scope
	bindings  []plannedBinding
	versions  map[*Scope]int
	committed bool
}

// plannedBinding is a single interface-to-concrete-type binding in a
// BindingPlan.
type plannedBinding struct {
	iface    reflect.Type
	concrete reflect.Type

	// Location of the constructor that produces the concrete type and the
	// path of the output within that constructor's results. These are
	// recorded for diagnostics.
	location *digreflect.Func
	path     string
}

// bindingCandidate is a concrete type visible in the container that may be
// bound to an interface.
type bindingCandidate struct {
	typ      reflect.Type
	location *digreflect.Func
	path     string
}

// PlanInterfaceBindings plans bindings for the given interfaces against the
// concrete types produced by constructors currently visible to the
// container. Each argument must be a pointer to an interface, e.g.
// new(io.Reader).
//
// For every requested interface, the plan looks for the unique unnamed,
// non-grouped concrete output type visible to the container that is
// assignable to the interface. Assignability follows Go method sets
// exactly: a value of type T is a candidate only if T itself implements the
// interface; the container never takes the address of a value to make it
// match. Interfaces that already have an explicit provider (for example,
// via [As]) are left untouched: the existing provider takes priority and no
// binding is planned for them.
//
// Planning inspects constructor signatures only; no constructor is
// executed. The returned plan is installed atomically with
// [BindingPlan.Commit]: either all bindings are installed, or none are.
//
// Planning fails if an interface has no candidate, or more than one
// candidate. In the latter case, disambiguate manually with [As] on the
// desired constructor and plan again.
func (c *Container) PlanInterfaceBindings(ifaces ...interface{}) (*BindingPlan, error) {
	return c.scope.PlanInterfaceBindings(ifaces...)
}

// PlanInterfaceBindings plans bindings for the given interfaces against the
// concrete types produced by constructors visible to this Scope. See
// [Container.PlanInterfaceBindings] for the full semantics.
//
// Visibility and shadowing follow the same rules as dependency resolution:
// providers registered with this Scope or any of its ancestors are visible,
// and a provider registered with a closer Scope shadows providers for the
// same type registered with its ancestors.
func (s *Scope) PlanInterfaceBindings(ifaces ...interface{}) (*BindingPlan, error) {
	if len(ifaces) == 0 {
		return nil, newErrInvalidInput(
			"must specify at least one interface to bind", nil)
	}

	targets := make([]reflect.Type, 0, len(ifaces))
	seen := make(map[reflect.Type]struct{}, len(ifaces))
	for _, i := range ifaces {
		t := reflect.TypeOf(i)
		if t == nil {
			return nil, newErrInvalidInput(
				"invalid interface binding target: argument must be a pointer to an interface", nil)
		}
		if t.Kind() != reflect.Ptr {
			return nil, newErrInvalidInput(
				fmt.Sprintf("invalid interface binding target %v: argument must be a pointer to an interface", t), nil)
		}
		iface := t.Elem()
		if iface.Kind() != reflect.Interface {
			return nil, newErrInvalidInput(
				fmt.Sprintf("invalid interface binding target *%v: argument must be a pointer to an interface", iface), nil)
		}
		if _, ok := seen[iface]; ok {
			return nil, newErrInvalidInput(
				fmt.Sprintf("interface %v specified more than once", iface), nil)
		}
		seen[iface] = struct{}{}
		targets = append(targets, iface)
	}

	plan := &BindingPlan{
		scope:    s,
		versions: make(map[*Scope]int),
	}
	for _, sc := range s.ancestors() {
		plan.versions[sc] = sc.provideVersion
	}

	for _, iface := range targets {
		// An existing explicit provider for the interface takes priority;
		// no binding is needed for it.
		if len(s.getAllValueProviders("", iface)) > 0 {
			continue
		}

		candidates := s.findBindingCandidates(iface)
		switch len(candidates) {
		case 0:
			return nil, errNoBindingCandidate{iface: iface}
		case 1:
			c := candidates[0]
			plan.bindings = append(plan.bindings, plannedBinding{
				iface:    iface,
				concrete: c.typ,
				location: c.location,
				path:     c.path,
			})
		default:
			return nil, errAmbiguousBinding{iface: iface, candidates: candidates}
		}
	}
	return plan, nil
}

// findBindingCandidates returns all unnamed, non-grouped concrete output
// types visible to the Scope that are assignable to iface, along with the
// constructors that produce them. Providers of a closer Scope shadow
// providers for the same type registered with its ancestors.
func (s *Scope) findBindingCandidates(iface reflect.Type) []bindingCandidate {
	var candidates []bindingCandidate
	shadowed := make(map[key]struct{})
	for _, scope := range s.ancestors() {
		keys := make([]key, 0, len(scope.providers))
		for k := range scope.providers {
			keys = append(keys, k)
		}
		// Sort for deterministic diagnostics.
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })

		for _, k := range keys {
			// Named values and value groups are never candidates for
			// unnamed interface bindings.
			if k.name != "" || k.group != "" {
				continue
			}
			if _, ok := shadowed[k]; ok {
				continue
			}
			shadowed[k] = struct{}{}

			// Only concrete types are candidates. Interface-typed
			// outputs (e.g. from dig.As) are explicit interface
			// providers, not binding candidates.
			if k.t.Kind() == reflect.Interface {
				continue
			}
			// Method sets are honored as-is: no automatic address-of.
			if !k.t.Implements(iface) {
				continue
			}
			for _, n := range scope.providers[k] {
				candidates = append(candidates, bindingCandidate{
					typ:      k.t,
					location: n.Location(),
					path:     findResultPath(n.ResultList(), k),
				})
			}
		}
	}
	return candidates
}

// findResultPath returns the path of the output that produces the given key
// within the constructor's results, e.g. "[0]" or "[1].Reader".
func findResultPath(rl resultList, k key) string {
	v := resultKeyPathVisitor{paths: make(map[key]string)}
	walkResult(rl, v)
	return v.paths[k]
}

// resultKeyPathVisitor records the output path of every key produced by a
// result tree.
type resultKeyPathVisitor struct {
	paths             map[key]string
	currentResultPath []string
}

func (v resultKeyPathVisitor) AnnotateWithField(f resultObjectField) resultVisitor {
	v.currentResultPath = append(v.currentResultPath, f.FieldName)
	return v
}

func (v resultKeyPathVisitor) AnnotateWithPosition(i int) resultVisitor {
	v.currentResultPath = append(v.currentResultPath, fmt.Sprintf("[%d]", i))
	return v
}

func (v resultKeyPathVisitor) Visit(res result) resultVisitor {
	path := strings.Join(v.currentResultPath, ".")
	switch r := res.(type) {
	case resultSingle:
		v.paths[key{name: r.Name, t: r.Type}] = path
		for _, asType := range r.As {
			v.paths[key{name: r.Name, t: asType}] = path
		}
	case resultGrouped:
		v.paths[key{group: r.Group, t: r.Type}] = path
		for _, asType := range r.As {
			v.paths[key{group: r.Group, t: asType}] = path
		}
	}
	return v
}

// Commit installs all bindings in the plan atomically: if any part of the
// installation fails, no binding is left behind.
//
// Commit fails if a constructor was registered with a Scope visible to the
// plan after the plan was created, or if installing the bindings would
// introduce a cycle into the dependency graph. Constructors are not
// executed during Commit.
func (p *BindingPlan) Commit() (err error) {
	if p.committed {
		return newErrInvalidInput("binding plan has already been committed", nil)
	}

	s := p.scope
	for sc, version := range p.versions {
		if sc.provideVersion != version {
			return errStaleBindingPlan{}
		}
	}
	p.committed = true

	if len(p.bindings) == 0 {
		return nil
	}

	// For all scopes affected by this change, take a snapshot of the
	// current graph state before we start making changes to it as we may
	// need to undo them upon encountering errors.
	allScopes := s.appendSubscopes(nil)
	defer func() {
		if err != nil {
			for _, sc := range allScopes {
				sc.gh.Rollback()
			}
		}
	}()
	for _, sc := range allScopes {
		sc.gh.Snapshot()
	}

	oldProviders := make(map[key][]*constructorNode)
	defer func() {
		if err != nil {
			for k, ops := range oldProviders {
				s.providers[k] = ops
			}
		}
	}()

	var nodes []*constructorNode
	for _, b := range p.bindings {
		var n *constructorNode
		n, err = newConstructorNode(
			makeInterfaceBindingFunc(b.concrete, b.iface),
			s,
			s,
			constructorOptions{
				Location: &digreflect.Func{
					Package: "go.uber.org/dig",
					Name:    fmt.Sprintf("interfacebinding(%v as %v)", b.concrete, b.iface),
				},
			},
		)
		if err != nil {
			return err
		}
		nodes = append(nodes, n)

		k := key{t: b.iface}
		if _, ok := oldProviders[k]; !ok {
			oldProviders[k] = s.providers[k]
		}
		s.providers[k] = append(s.providers[k], n)
	}

	for _, sc := range allScopes {
		sc.isVerifiedAcyclic = false
		if sc.deferAcyclicVerification {
			continue
		}
		if ok, cycle := graph.IsAcyclic(sc.gh); !ok {
			err = errBindingCycle{
				bindings: p.bindings,
				cause:    sc.cycleDetectedError(cycle),
			}
			return err
		}
		sc.isVerifiedAcyclic = true
	}

	s.nodes = append(s.nodes, nodes...)
	s.provideVersion++
	return nil
}

// makeInterfaceBindingFunc builds a constructor of the shape
// func(concrete) iface that returns its input. The concrete value is
// produced by the constructor already registered for it, so binding
// introduces no new values, only a new view of an existing one.
func makeInterfaceBindingFunc(concrete, iface reflect.Type) interface{} {
	ftype := reflect.FuncOf([]reflect.Type{concrete}, []reflect.Type{iface}, false)
	return reflect.MakeFunc(ftype, func(args []reflect.Value) []reflect.Value {
		out := reflect.New(iface).Elem()
		out.Set(args[0])
		return []reflect.Value{out}
	}).Interface()
}

// errNoBindingCandidate is returned when no visible provider output can be
// bound to a requested interface.
type errNoBindingCandidate struct {
	iface reflect.Type
}

var _ digError = errNoBindingCandidate{}

func (e errNoBindingCandidate) Error() string { return fmt.Sprint(e) }

func (e errNoBindingCandidate) writeMessage(w io.Writer, _ string) {
	fmt.Fprintf(w,
		"no candidate for interface %v: no visible unnamed provider output is assignable to it", e.iface)
}

func (e errNoBindingCandidate) Format(w fmt.State, c rune) {
	formatError(e, w, c)
}

// errAmbiguousBinding is returned when more than one visible provider
// output can be bound to a requested interface.
type errAmbiguousBinding struct {
	iface      reflect.Type
	candidates []bindingCandidate
}

var _ digError = errAmbiguousBinding{}

func (e errAmbiguousBinding) Error() string { return fmt.Sprint(e) }

func (e errAmbiguousBinding) writeMessage(w io.Writer, v string) {
	fmt.Fprintf(w, "multiple candidates for interface %v:", e.iface)
	for _, c := range e.candidates {
		fmt.Fprintf(w, "\n\t%v provided by "+v+" (output %v)", c.typ, c.location, c.path)
	}
}

func (e errAmbiguousBinding) Format(w fmt.State, c rune) {
	formatError(e, w, c)
}

// errStaleBindingPlan is returned when a BindingPlan is committed after new
// constructors were registered with a Scope visible to the plan.
type errStaleBindingPlan struct{}

var _ digError = errStaleBindingPlan{}

func (e errStaleBindingPlan) Error() string { return fmt.Sprint(e) }

func (e errStaleBindingPlan) writeMessage(w io.Writer, _ string) {
	fmt.Fprint(w,
		"binding plan is stale: constructors were registered after the plan was created")
}

func (e errStaleBindingPlan) Format(w fmt.State, c rune) {
	formatError(e, w, c)
}

// errBindingCycle is returned when installing the planned bindings would
// introduce a cycle into the dependency graph.
type errBindingCycle struct {
	bindings []plannedBinding
	cause    error
}

var _ digError = errBindingCycle{}

func (e errBindingCycle) Error() string { return fmt.Sprint(e) }

func (e errBindingCycle) Unwrap() error { return e.cause }

func (e errBindingCycle) writeMessage(w io.Writer, v string) {
	io.WriteString(w, "interface bindings introduce a cycle:")
	for _, b := range e.bindings {
		fmt.Fprintf(w, "\n\t%v bound to %v provided by "+v+" (output %v)",
			b.iface, b.concrete, b.location, b.path)
	}
}

func (e errBindingCycle) Format(w fmt.State, c rune) {
	formatError(e, w, c)
}
