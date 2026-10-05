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
	"bytes"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"time"

	"go.uber.org/dig/internal/digclock"
)

// A ScopeOption modifies the default behavior of Scope; currently,
// there are no implementations.
type ScopeOption interface {
	noScopeOption() // yet
}

// Scope is a scoped DAG of types and their dependencies.
// A Scope may also have one or more child Scopes that inherit
// from it.
type Scope struct {
	// This implements containerStore interface.

	// Name of the Scope
	name string
	// Mapping from key to all the constructor node that can provide a value for that
	// key.
	providers map[key][]*constructorNode

	// Mapping from key to all the decorator nodes that decorate a value for
	// that key. Nodes are ordered by their stage within this Scope: legacy
	// decorators registered with Decorate occupy stage zero, and decorators
	// registered with a DecorationChain follow in declaration order.
	decorators map[key][]*decoratorNode

	// constructorNodes provided directly to this Scope. i.e. it does not include
	// any nodes that were provided to the parent Scope this inherited from.
	nodes []*constructorNode

	// Values that generated via decorators in the Scope.
	decoratedValues map[key]reflect.Value

	// Values that generated directly in the Scope.
	values map[key]reflect.Value

	// Values groups that generated directly in the Scope.
	groups map[key][]reflect.Value

	// Values groups that generated via decoraters in the Scope.
	decoratedGroups map[key]reflect.Value

	// Stack of decorator nodes that are currently being called. This is used
	// to detect cycles between decorators and to report the involved stages.
	decoratorStack []*decoratorNode

	// Number of stages registered for each decoration chain name. This
	// assigns the stable 1-based stage position to each chain decorator.
	chainStages map[string]int

	// Stage names registered for each decoration chain, in declaration
	// order. Used to reject duplicate stage names within a chain.
	chainStageNames map[string][]string

	// Source of randomness.
	rand *rand.Rand

	// Flag indicating whether the graph has been checked for cycles.
	isVerifiedAcyclic bool

	// Defer acyclic check on provide until Invoke.
	deferAcyclicVerification bool

	// Recover from panics in user-provided code and wrap in an exported error type.
	recoverFromPanics bool

	// invokerFn calls a function with arguments provided to Provide or Invoke.
	invokerFn invokerFn

	// graph of this Scope. Note that this holds the dependency graph of all the
	// nodes that affect this Scope, not just the ones provided directly to this Scope.
	gh *graphHolder

	// Parent of this Scope.
	parentScope *Scope

	// All the child scopes of this Scope.
	childScopes []*Scope

	// clockSrc stores the source of time. Defaults to system clock.
	clockSrc digclock.Clock
}

func newScope() *Scope {
	s := &Scope{
		providers:       make(map[key][]*constructorNode),
		decorators:      make(map[key][]*decoratorNode),
		values:          make(map[key]reflect.Value),
		decoratedValues: make(map[key]reflect.Value),
		groups:          make(map[key][]reflect.Value),
		decoratedGroups: make(map[key]reflect.Value),
		chainStages:     make(map[string]int),
		chainStageNames: make(map[string][]string),
		invokerFn:       defaultInvoker,
		rand:            rand.New(rand.NewSource(time.Now().UnixNano())),
		clockSrc:        digclock.System,
	}
	s.gh = newGraphHolder(s)
	return s
}

// Scope creates a new Scope with the given name and options from current Scope.
// Any constructors that the current Scope knows about, as well as any modifications
// made to it in the future will be propagated to the child scope.
// However, no modifications made to the child scope being created will be propagated
// to the parent Scope.
func (s *Scope) Scope(name string, opts ...ScopeOption) *Scope {
	child := newScope()
	child.name = name
	child.parentScope = s
	child.invokerFn = s.invokerFn
	child.clockSrc = s.clockSrc
	child.deferAcyclicVerification = s.deferAcyclicVerification
	child.recoverFromPanics = s.recoverFromPanics

	// child copies the parent's graph nodes.
	for _, node := range s.gh.nodes {
		child.gh.nodes = append(child.gh.nodes, node)
		if ctrNode, ok := node.Wrapped.(*constructorNode); ok {
			ctrNode.CopyOrder(s, child)
		}
	}

	for _, opt := range opts {
		opt.noScopeOption()
	}

	s.childScopes = append(s.childScopes, child)
	return child
}

// ancestors returns a list of scopes of ancestors of this scope up to the
// root. The scope at at index 0 is this scope itself.
func (s *Scope) ancestors() []*Scope {
	var scopes []*Scope
	for s := s; s != nil; s = s.parentScope {
		scopes = append(scopes, s)
	}
	return scopes
}

func (s *Scope) appendSubscopes(dest []*Scope) []*Scope {
	dest = append(dest, s)
	for _, cs := range s.childScopes {
		dest = cs.appendSubscopes(dest)
	}
	return dest
}

func (s *Scope) storesToRoot() []containerStore {
	scopes := s.ancestors()
	stores := make([]containerStore, len(scopes))
	for i, s := range scopes {
		stores[i] = s
	}
	return stores
}

func (s *Scope) knownTypes() []reflect.Type {
	typeSet := make(map[reflect.Type]struct{}, len(s.providers))
	for k := range s.providers {
		typeSet[k.t] = struct{}{}
	}

	types := make([]reflect.Type, 0, len(typeSet))
	for t := range typeSet {
		types = append(types, t)
	}
	sort.Sort(byTypeName(types))
	return types
}

func (s *Scope) getValue(name string, t reflect.Type) (v reflect.Value, ok bool) {
	v, ok = s.values[key{name: name, t: t}]
	return
}

func (s *Scope) getDecoratedValue(name string, t reflect.Type) (v reflect.Value, ok bool) {
	v, ok = s.decoratedValues[key{name: name, t: t}]
	return
}

func (s *Scope) setValue(name string, t reflect.Type, v reflect.Value) {
	s.values[key{name: name, t: t}] = v
}

func (s *Scope) setDecoratedValue(name string, t reflect.Type, v reflect.Value) {
	s.decoratedValues[key{name: name, t: t}] = v
}

func (s *Scope) getValueGroup(name string, t reflect.Type) []reflect.Value {
	items := s.groups[key{group: name, t: t}]
	// shuffle the list so users don't rely on the ordering of grouped values
	return shuffledCopy(s.rand, items)
}

func (s *Scope) getDecoratedValueGroup(name string, t reflect.Type) (reflect.Value, bool) {
	items, ok := s.decoratedGroups[key{group: name, t: t}]
	return items, ok
}

func (s *Scope) submitGroupedValue(name string, t reflect.Type, v reflect.Value) {
	k := key{group: name, t: t}
	s.groups[k] = append(s.groups[k], v)
}

func (s *Scope) submitDecoratedGroupedValue(name string, t reflect.Type, v reflect.Value) {
	k := key{group: name, t: t}
	s.decoratedGroups[k] = v
}

func (s *Scope) getValueProviders(name string, t reflect.Type) []provider {
	return s.getProviders(key{name: name, t: t})
}

func (s *Scope) getGroupProviders(name string, t reflect.Type) []provider {
	return s.getProviders(key{group: name, t: t})
}

func (s *Scope) getValueDecorator(name string, t reflect.Type) (decorator, bool) {
	return s.getLastDecorator(key{name: name, t: t})
}

func (s *Scope) getGroupDecorator(name string, t reflect.Type) (decorator, bool) {
	return s.getLastDecorator(key{group: name, t: t})
}

func (s *Scope) getLastDecorator(k key) (decorator, bool) {
	nodes := s.decorators[k]
	if len(nodes) == 0 {
		return nil, false
	}
	return nodes[len(nodes)-1], true
}

func (s *Scope) getValueDecorators(name string, t reflect.Type) []*decoratorNode {
	return s.decorators[key{name: name, t: t}]
}

func (s *Scope) getGroupDecorators(name string, t reflect.Type) []*decoratorNode {
	return s.decorators[key{group: name, t: t}]
}

// getNodeDecoratedValue returns the value produced for the given key by the
// given decorator node. Each decorator stage keeps the value it produced
// separately, so intermediate results of a chain never overwrite each other.
func (s *Scope) getNodeDecoratedValue(n *decoratorNode, k key) (reflect.Value, bool) {
	v, ok := n.outputs[k]
	return v, ok
}

// nextChainStage returns the position that the next decorator of the given
// decoration chain would take.
func (s *Scope) nextChainStage(chainName string) int {
	return s.chainStages[chainName] + 1
}

// checkChainStageName rejects duplicate stage names within the same chain.
func (s *Scope) checkChainStageName(chainName, stageName string) error {
	for _, name := range s.chainStageNames[chainName] {
		if name == stageName {
			return newErrInvalidInput(
				fmt.Sprintf("stage %q is already registered in decoration chain %q", stageName, chainName), nil)
		}
	}
	return nil
}

// commitChainStage commits the stage position and records the stage name.
func (s *Scope) commitChainStage(chainName, stageName string) {
	s.chainStages[chainName]++
	s.chainStageNames[chainName] = append(s.chainStageNames[chainName], stageName)
}

func (s *Scope) getProviders(k key) []provider {
	nodes := s.providers[k]
	providers := make([]provider, len(nodes))
	for i, n := range nodes {
		providers[i] = n
	}
	return providers
}

func (s *Scope) getAllGroupProviders(name string, t reflect.Type) []provider {
	return s.getAllProviders(key{group: name, t: t})
}

func (s *Scope) getAllValueProviders(name string, t reflect.Type) []provider {
	return s.getAllProviders(key{name: name, t: t})
}

func (s *Scope) getAllProviders(k key) []provider {
	allScopes := s.ancestors()
	var providers []provider
	for _, scope := range allScopes {
		providers = append(providers, scope.getProviders(k)...)
	}
	return providers
}

func (s *Scope) invoker() invokerFn {
	return s.invokerFn
}

func (s *Scope) clock() digclock.Clock {
	return s.clockSrc
}

// adds a new graphNode to this Scope and all of its descendent
// scope.
func (s *Scope) newGraphNode(wrapped interface{}, orders map[*Scope]int) {
	orders[s] = s.gh.NewNode(wrapped)
	for _, cs := range s.childScopes {
		cs.newGraphNode(wrapped, orders)
	}
}

func (s *Scope) cycleDetectedError(cycle []int) error {
	var path []cycleErrPathEntry
	for _, n := range cycle {
		if n, ok := s.gh.Lookup(n).(*constructorNode); ok {
			path = append(path, cycleErrPathEntry{
				Key: key{
					t: n.CType(),
				},
				Func: n.Location(),
			})
		}
	}
	return errCycleDetected{Path: path, scope: s}
}

// Returns the root Scope that can be reached from this Scope.
func (s *Scope) rootScope() *Scope {
	curr := s
	for curr.parentScope != nil {
		curr = curr.parentScope
	}
	return curr
}

// String representation of the entire Scope
func (s *Scope) String() string {
	b := &bytes.Buffer{}
	fmt.Fprintln(b, "nodes: {")
	for k, vs := range s.providers {
		for _, v := range vs {
			fmt.Fprintln(b, "\t", k, "->", v)
		}
	}
	fmt.Fprintln(b, "}")

	fmt.Fprintln(b, "values: {")
	for k, v := range s.values {
		fmt.Fprintln(b, "\t", k, "=>", v)
	}
	for k, vs := range s.groups {
		for _, v := range vs {
			fmt.Fprintln(b, "\t", k, "=>", v)
		}
	}
	fmt.Fprintln(b, "}")

	return b.String()
}
