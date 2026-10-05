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
	"fmt"
	"reflect"

	"go.uber.org/dig/internal/digreflect"
	"go.uber.org/dig/internal/dot"
)

type decoratorState int

const (
	decoratorReady decoratorState = iota
	decoratorOnStack
	decoratorCalled
)

type decorator interface {
	Call(c containerStore) error
	ID() dot.CtorID
	State() decoratorState
}

type decoratorNode struct {
	dcor  interface{}
	dtype reflect.Type

	id dot.CtorID

	// Location where this function was defined.
	location *digreflect.Func

	// Current state of this decorator
	state decoratorState

	// Parameters of the decorator.
	params paramList

	// Results of the decorator.
	results resultList

	// Keys that this decorator produces. Used to distinguish between a stage
	// requesting its own output (which resolves to the previous stage) and a
	// different type depending back on a stage that is still running (which
	// is a real cycle).
	resultKeys []key

	// Name of the decoration chain this node belongs to. Empty for decorators
	// registered with the legacy Decorate API.
	chainName string

	// Human-readable name of the stage within the chain. Empty for legacy
	// decorators.
	chainStageLabel string

	// 1-based position of this node within its decoration chain in the Scope.
	// Zero for legacy decorators.
	chainStage int

	// Order of this node in each Scopes' graphHolders.
	orders map[*Scope]int

	// Scope this node was originally provided to.
	s *Scope

	// Outputs of this decorator keyed by result key. Every stage keeps its
	// own output so intermediate results of a decoration chain never
	// overwrite each other. The owning Scope's decoratedValues and
	// decoratedGroups maps additionally mirror the output of the last stage.
	outputs map[key]reflect.Value

	// Callback for this decorator, if there is one.
	callback Callback

	// BeforeCallback for this decorator, if there is one
	beforeCallback BeforeCallback
}

func newDecoratorNode(dcor interface{}, s *Scope, opts decorateOptions) (*decoratorNode, error) {
	dval := reflect.ValueOf(dcor)
	dtype := dval.Type()
	dptr := dval.Pointer()

	pl, err := newParamList(dtype, s)
	if err != nil {
		return nil, err
	}

	rl, err := newResultList(dtype, resultOptions{})
	if err != nil {
		return nil, err
	}

	keys, err := findResultKeys(rl)
	if err != nil {
		return nil, err
	}

	n := &decoratorNode{
		dcor:           dcor,
		dtype:          dtype,
		id:             dot.CtorID(dptr),
		location:       digreflect.InspectFunc(dcor),
		orders:         make(map[*Scope]int),
		params:         pl,
		results:        rl,
		resultKeys:     keys,
		s:              s,
		outputs:        make(map[key]reflect.Value),
		callback:       opts.Callback,
		beforeCallback: opts.BeforeCallback,
	}
	return n, nil
}

// hasResultKey reports whether the node produces the given key.
func (n *decoratorNode) hasResultKey(k key) bool {
	for _, rk := range n.resultKeys {
		if rk == k {
			return true
		}
	}
	return false
}

// stageIdentity returns a stable identifier of the stage represented by this
// decorator node. It is used in errors, in particular in cycle diagnostics.
func (n *decoratorNode) stageIdentity() string {
	if n.chainName == "" {
		return fmt.Sprintf("decorator %v", n.dtype)
	}
	return fmt.Sprintf("stage %q (#%d) of decoration chain %q (%v)",
		n.chainStageLabel, n.chainStage, n.chainName, n.dtype)
}

func (n *decoratorNode) Call(c containerStore) (err error) {
	if n.state == decoratorCalled {
		return nil
	}
	if n.state == decoratorOnStack {
		// A direct re-entrant call is always a cycle; param resolution
		// normally reports this before reaching here.
		return n.s.decoratorCycleError(n)
	}

	n.state = decoratorOnStack
	n.s.decoratorStack = append(n.s.decoratorStack, n)
	defer func() {
		n.s.decoratorStack = n.s.decoratorStack[:len(n.s.decoratorStack)-1]
	}()
	// A stage constructs at most once: on failure nothing is committed and
	// the node returns to the ready state so a later request can retry it.
	// Outputs of earlier, already-called stages stay cached.
	defer func() {
		if err != nil {
			n.state = decoratorReady
		}
	}()

	if err := shallowCheckDependencies(c, n.params); err != nil {
		return errMissingDependencies{
			Func:   n.location,
			Reason: err,
		}
	}

	args, err := n.params.BuildList(c)
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

	// Stage outputs are staged and only committed together once the whole
	// decorator function has succeeded. A failure halfway through must not
	// leave partial results behind, including for multi-output decorators.
	receiver := newStagingDecoratedWriter()
	results := c.invoker()(reflect.ValueOf(n.dcor), args)
	if err = n.results.ExtractList(receiver, true /* decorated */, results); err != nil {
		return err
	}
	receiver.Commit(n.s)
	for k, v := range receiver.values {
		n.outputs[k] = v
	}
	for k, v := range receiver.groups {
		n.outputs[k] = v
	}
	n.state = decoratorCalled
	return nil
}

func (n *decoratorNode) ID() dot.CtorID { return n.id }

func (n *decoratorNode) State() decoratorState { return n.state }

// DecorateOption modifies the default behavior of Decorate.
type DecorateOption interface {
	apply(*decorateOptions)
}

type decorateOptions struct {
	Info           *DecorateInfo
	Callback       Callback
	BeforeCallback BeforeCallback
}

// FillDecorateInfo is a DecorateOption that writes info on what Dig was
// able to get out of the provided decorator into the provided DecorateInfo.
func FillDecorateInfo(info *DecorateInfo) DecorateOption {
	return fillDecorateInfoOption{info: info}
}

type fillDecorateInfoOption struct{ info *DecorateInfo }

func (o fillDecorateInfoOption) String() string {
	return fmt.Sprintf("FillDecorateInfo(%p)", o.info)
}

func (o fillDecorateInfoOption) apply(opts *decorateOptions) {
	opts.Info = o.info
}

// DecorateInfo provides information about the decorator's inputs and outputs
// types as strings, as well as the ID of the decorator supplied to the Container.
type DecorateInfo struct {
	ID      ID
	Inputs  []*Input
	Outputs []*Output
}

// Decorate provides a decorator for a type that has already been provided in the Container.
// Decorations at this level affect all scopes of the container.
// See Scope.Decorate for information on how to use this method.
func (c *Container) Decorate(decorator interface{}, opts ...DecorateOption) error {
	return c.scope.Decorate(decorator, opts...)
}

// DecorationChain returns a named decoration chain for the Container. Use the
// returned object to register multiple decorators for the same type, each
// identified by a stable stage name.
func (c *Container) DecorationChain(name string) *DecorationChain {
	return c.scope.DecorationChain(name)
}

// Decorate provides a decorator for a type that has already been provided in the Scope.
//
// Similar to Provide, Decorate takes in a function with zero or more dependencies and one
// or more results. Decorate can be used to modify a type that was already introduced to the
// Scope, or completely replace it with a new object.
//
// For example,
//
//	s.Decorate(func(log *zap.Logger) *zap.Logger {
//	  return log.Named("myapp")
//	})
//
// This takes in a value, augments it with a name, and returns a replacement for it. Functions
// in the Scope's dependency graph that use *zap.Logger will now use the *zap.Logger
// returned by this decorator.
//
// A decorator can also take in multiple parameters and replace one of them:
//
//	s.Decorate(func(log *zap.Logger, cfg *Config) *zap.Logger {
//	  return log.Named(cfg.Name)
//	})
//
// Or replace a subset of them:
//
//	s.Decorate(func(
//	  log *zap.Logger,
//	  cfg *Config,
//	  scope metrics.Scope
//	) (*zap.Logger, metrics.Scope) {
//	  log = log.Named(cfg.Name)
//	  scope = scope.With(metrics.Tag("service", cfg.Name))
//	  return log, scope
//	})
//
// Decorating a Scope affects all the child scopes of this Scope.
//
// Similar to a provider, the decorator function gets called *at most once*.
func (s *Scope) Decorate(decorator interface{}, opts ...DecorateOption) error {
	var options decorateOptions
	for _, opt := range opts {
		opt.apply(&options)
	}

	return s.registerDecorator(decorator, options, "", "", 0)
}

// DecorationChain returns a named decoration chain for this Scope.
//
// A decoration chain allows registering several decorators for the same type
// within a single Scope. Decorators added to the same chain run in the order
// in which they were declared: the first stage receives the undecorated value
// and every subsequent stage receives the output of the previous stage.
//
// Stages are identified by the name of the chain plus a 1-based stage
// position. The identity is stable and included in diagnostics, for example
// when a cycle between decorators is detected.
//
// A decorator may produce multiple results; all of them are committed
// together when the stage succeeds, so other stages in the same declaration
// can observe each other's outputs according to the usual dependency rules.
// A stage that fails commits nothing, and it may be retried on a later
// request; earlier stages are not re-run.
//
//	chain := s.DecorationChain("logger")
//	chain.Decorate("request-tag", func(log *Logger) *Logger { ... })
//	chain.Decorate("audit", func(log *Logger) *Logger { ... })
//
// Types produced by a stage are visible in the Scope exactly as if they were
// decorated once: named values and value groups keep their key semantics,
// and child Scopes inherit or shadow the chain following the same rules that
// apply to single decorators.
func (s *Scope) DecorationChain(name string) *DecorationChain {
	return &DecorationChain{name: name, s: s}
}

// DecorationChain is a named entry point for registering multiple decorators
// for the same types in one Scope.
//
// A DecorationChain must not be constructed directly; use
// Scope.DecorationChain to get one.
type DecorationChain struct {
	name string
	s    *Scope
}

// Name returns the name of the decoration chain.
func (dc *DecorationChain) Name() string {
	return dc.name
}

// Decorate registers a decorator as a new stage of the chain. The stage
// argument is a human-readable identifier for the stage; together with the
// chain name and the 1-based position it forms the stage's stable identity,
// which is reported in diagnostics.
//
// The same stage name may not be registered twice within the chain.
func (dc *DecorationChain) Decorate(stage string, decorator interface{}, opts ...DecorateOption) error {
	if stage == "" {
		return newErrInvalidInput(
			fmt.Sprintf("cannot register decorator in decoration chain %q: stage name must not be empty", dc.name), nil)
	}

	var options decorateOptions
	for _, opt := range opts {
		opt.apply(&options)
	}

	if err := dc.s.checkChainStageName(dc.name, stage); err != nil {
		return err
	}
	stagePos := dc.s.nextChainStage(dc.name)
	if err := dc.s.registerDecorator(decorator, options, dc.name, stage, stagePos); err != nil {
		return err
	}

	dc.s.commitChainStage(dc.name, stage)
	return nil
}

// registerDecorator builds a decorator node and registers it for all keys it
// produces. chainName is empty and stage is zero for legacy Decorate calls;
// such calls retain the original behavior of rejecting a second decorator for
// any produced key in this Scope.
func (s *Scope) registerDecorator(decorator interface{}, options decorateOptions, chainName, stageLabel string, stage int) error {
	dn, err := newDecoratorNode(decorator, s, options)
	if err != nil {
		return err
	}
	dn.chainName = chainName
	dn.chainStageLabel = stageLabel
	dn.chainStage = stage

	for _, k := range dn.resultKeys {
		nodes := s.decorators[k]
		if chainName == "" {
			// Legacy Decorate retains its original behavior: a key may have
			// at most one decorator, whether it came from Decorate or from
			// a chain.
			if len(nodes) > 0 {
				return newErrInvalidInput(
					fmt.Sprintf("cannot decorate using function %v: %s already decorated", dn.dtype, k), nil)
			}
			continue
		}
		for _, existing := range nodes {
			if existing.chainName == "" {
				// A chain cannot extend a key already decorated through the
				// legacy single-Decorate API within this Scope.
				return newErrInvalidInput(
					fmt.Sprintf("cannot decorate using function %v: %s already decorated", dn.dtype, k), nil)
			}
			if existing.chainName == chainName && existing.chainStage == stage {
				return newErrInvalidInput(
					fmt.Sprintf("cannot decorate using function %v: %s already decorated by %s",
						dn.dtype, k, existing.stageIdentity()), nil)
			}
		}
	}

	for _, k := range dn.resultKeys {
		s.decorators[k] = append(s.decorators[k], dn)
	}

	if info := options.Info; info != nil {
		params := dn.params.DotParam()
		results := dn.results.DotResult()
		info.ID = (ID)(dn.id)
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

func findResultKeys(r resultList) ([]key, error) {
	// use BFS to search for all keys included in a resultList.
	var (
		q    []result
		keys []key
	)
	q = append(q, r)

	for len(q) > 0 {
		res := q[0]
		q = q[1:]

		switch innerResult := res.(type) {
		case resultSingle:
			keys = append(keys, key{t: innerResult.Type, name: innerResult.Name})
		case resultGrouped:
			if innerResult.Type.Kind() != reflect.Slice {
				return nil, newErrInvalidInput("decorating a value group requires decorating the entire value group, not a single value", nil)
			}
			keys = append(keys, key{t: innerResult.Type.Elem(), group: innerResult.Group})
		case resultObject:
			for _, f := range innerResult.Fields {
				q = append(q, f.Result)
			}
		case resultList:
			q = append(q, innerResult.Results...)
		}
	}
	return keys, nil
}
