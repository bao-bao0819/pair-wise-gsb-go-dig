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

	"go.uber.org/dig/internal/digerror"
	"go.uber.org/dig/internal/digreflect"
	"go.uber.org/dig/internal/dot"
)

// constructorNode is a node in the dependency graph that represents
// a constructor provided by the user.
//
// constructorNodes can produce zero or more values that they store into the container.
// For the Provide path, we verify that constructorNodes produce at least one value,
// otherwise the function will never be called.
type constructorNode struct {
	ctor  interface{}
	ctype reflect.Type

	// Location where this function was defined.
	location *digreflect.Func

	// id uniquely identifies the constructor that produces a node.
	id dot.CtorID

	// Whether the constructor owned by this node was already called.
	called bool

	// Type information about constructor parameters.
	paramList paramList

	// Type information about constructor results.
	resultList resultList

	// Order of this node in each Scopes' graphHolders.
	orders map[*Scope]int

	// Scope this node is part of.
	s *Scope

	// Scope this node was originally provided to.
	// This is different from s if and only if the constructor was Provided with ExportOption.
	origS *Scope

	// Contiguous blocks produced by this constructor for each value group,
	// populated when the node is called. Used by ordered value-group
	// consumers; ordinary consumers read from the Scope's group storage.
	groupResults map[key][]groupBlock

	// Callback for this provided function, if there is one.
	callback Callback

	// BeforeCallback for this provided function, if there is one.
	beforeCallback BeforeCallback
}

type constructorOptions struct {
	// If specified, all values produced by this constructor have the provided name
	// belong to the specified value group or implement any of the interfaces.
	ResultName     string
	ResultGroup    string
	ResultGroupTag string
	ResultAs       []interface{}
	Location       *digreflect.Func
	Callback       Callback
	BeforeCallback BeforeCallback
}

func newConstructorNode(ctor interface{}, s *Scope, origS *Scope, opts constructorOptions) (*constructorNode, error) {
	cval := reflect.ValueOf(ctor)
	ctype := cval.Type()
	cptr := cval.Pointer()

	params, err := newParamList(ctype, s)
	if err != nil {
		return nil, err
	}

	results, err := newResultList(
		ctype,
		resultOptions{
			Name:     opts.ResultName,
			Group:    opts.ResultGroup,
			GroupTag: opts.ResultGroupTag,
			As:       opts.ResultAs,
		},
	)
	if err != nil {
		return nil, err
	}

	location := opts.Location
	if location == nil {
		location = digreflect.InspectFunc(ctor)
	}

	n := &constructorNode{
		ctor:           ctor,
		ctype:          ctype,
		location:       location,
		id:             dot.CtorID(cptr),
		paramList:      params,
		resultList:     results,
		orders:         make(map[*Scope]int),
		s:              s,
		origS:          origS,
		callback:       opts.Callback,
		beforeCallback: opts.BeforeCallback,
	}
	s.newGraphNode(n, n.orders)
	return n, nil
}

func (n *constructorNode) Location() *digreflect.Func { return n.location }
func (n *constructorNode) ParamList() paramList       { return n.paramList }
func (n *constructorNode) ResultList() resultList     { return n.resultList }
func (n *constructorNode) ID() dot.CtorID             { return n.id }
func (n *constructorNode) CType() reflect.Type        { return n.ctype }
func (n *constructorNode) Order(s *Scope) int         { return n.orders[s] }
func (n *constructorNode) OrigScope() *Scope          { return n.origS }

// CopyOrder copies the order for the given parent scope to the given child scope.
func (n *constructorNode) CopyOrder(parent, child *Scope) {
	n.orders[child] = n.orders[parent]
}

func (n *constructorNode) String() string {
	return fmt.Sprintf("deps: %v, ctor: %v", n.paramList, n.ctype)
}

// Call calls this constructor if it hasn't already been called and
// injects any values produced by it into the provided container.
func (n *constructorNode) Call(c containerStore) (err error) {
	if n.called {
		return nil
	}

	if err := shallowCheckDependencies(c, n.paramList); err != nil {
		return errMissingDependencies{
			Func:   n.location,
			Reason: err,
		}
	}

	args, err := n.paramList.BuildList(c)
	if err != nil {
		return errArgumentsFailed{
			Func:   n.location,
			Reason: err,
		}
	}

	if n.beforeCallback != nil {
		n.beforeCallback(BeforeCallbackInfo{
			Name: fmt.Sprintf("%v.%v", n.location.Package, n.location.Name),
		})
	}

	if n.callback != nil {
		start := c.clock().Now()
		// Wrap in separate func to include PanicErrors
		defer func() {
			n.callback(CallbackInfo{
				Name:    fmt.Sprintf("%v.%v", n.location.Package, n.location.Name),
				Error:   err,
				Runtime: c.clock().Since(start),
			})
		}()
	}

	if n.s.recoverFromPanics {
		defer func() {
			if p := recover(); p != nil {
				err = PanicError{
					fn:    n.location,
					Panic: p,
				}
			}
		}()
	}

	receiver := newStagingContainerWriter()
	receiver.node = n
	results := c.invoker()(reflect.ValueOf(n.ctor), args)
	if err = n.resultList.ExtractList(receiver, false /* decorating */, results); err != nil {
		return errConstructorFailed{Func: n.location, Reason: err}
	}

	// Commit the result to the original container that this constructor
	// was supplied to. The provided constructor is only used for a view of
	// the rest of the graph to instantiate the dependencies of this
	// container.
	receiver.Commit(n.s)
	receiver.commitGroupResults()
	n.called = true
	return nil
}

// stagingContainerWriter is a containerWriter that records the changes that
// would be made to a containerWriter and defers them until Commit is called.
type stagingContainerWriter struct {
	values map[key]reflect.Value
	groups map[key][]reflect.Value

	// groupBlocks records, per group/element-type key, the contiguous blocks
	// produced by this constructor in result-extraction order. It drives
	// ordered value-group consumers.
	groupBlocks map[key][]groupBlock

	// blockSeq is a constructor-wide counter identifying every grouped
	// result in extraction order, across all element types and groups. It
	// aligns the static plan with values captured after the call.
	blockSeq int

	// node is the constructor whose results this writer is staging.
	node *constructorNode
}

// groupBlock is one contiguous contribution to a value group made by a
// constructor: a single non-flattened member, or all elements of one
// flattened slice. Tag is the declared member tag (possibly empty), and Seq
// is the constructor-wide index of the originating grouped result.
type groupBlock struct {
	Tag    string
	Seq    int
	Values []reflect.Value
}

var _ containerWriter = (*stagingContainerWriter)(nil)

func newStagingContainerWriter() *stagingContainerWriter {
	return &stagingContainerWriter{
		values:      make(map[key]reflect.Value),
		groups:      make(map[key][]reflect.Value),
		groupBlocks: make(map[key][]groupBlock),
	}
}

func (sr *stagingContainerWriter) setValue(name string, t reflect.Type, v reflect.Value) {
	sr.values[key{t: t, name: name}] = v
}

func (sr *stagingContainerWriter) setDecoratedValue(_ string, _ reflect.Type, _ reflect.Value) {
	digerror.BugPanicf("stagingContainerWriter.setDecoratedValue must never be called")
}

func (sr *stagingContainerWriter) submitGroupedBlock(group string, t reflect.Type, tag string, values []reflect.Value) {
	k := key{t: t, group: group}
	sr.groups[k] = append(sr.groups[k], values...)
	sr.groupBlocks[k] = append(sr.groupBlocks[k], groupBlock{Tag: tag, Seq: sr.blockSeq, Values: values})
	sr.blockSeq++
}

func (sr *stagingContainerWriter) submitGroupedValueAs(group string, t reflect.Type, values []reflect.Value) {
	k := key{t: t, group: group}
	sr.groups[k] = append(sr.groups[k], values...)
	// The alternate (dig.As) element type shares the member's seq via the
	// originating grouped result; ordered consumers discover it through the
	// static plan and read values from the primary type's block, so only the
	// unordered storage needs an entry here.
}

func (sr *stagingContainerWriter) submitDecoratedGroupedValue(_ string, _ reflect.Type, _ reflect.Value) {
	digerror.BugPanicf("stagingContainerWriter.submitDecoratedGroupedValue must never be called")
}

// Commit commits the received results to the provided containerWriter.
func (sr *stagingContainerWriter) Commit(cw containerWriter) {
	for k, v := range sr.values {
		cw.setValue(k.name, k.t, v)
	}

	for k, vs := range sr.groups {
		for _, v := range vs {
			cw.submitGroupedBlock(k.group, k.t, "", []reflect.Value{v})
		}
	}
}

// commitGroupResults stores the blocks produced by this constructor on the
// constructor node for ordered value-group consumers.
func (sr *stagingContainerWriter) commitGroupResults() {
	if sr.node == nil {
		return
	}
	if sr.node.groupResults == nil {
		sr.node.groupResults = make(map[key][]groupBlock)
	}
	for k, blocks := range sr.groupBlocks {
		sr.node.groupResults[k] = append(sr.node.groupResults[k], blocks...)
	}
}
