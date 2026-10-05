package dig

import (
	"bytes"
	"fmt"
	"io"
	"reflect"
)

// errDecoratorCycleDetected is returned when a cycle is detected between
// decorator stages while resolving a value. Unlike constructor cycles, which
// are detected up-front from the static graph, decorator cycles can only be
// observed at build time because a stage's input of its own output is a
// legitimate request for the previous stage's result.
type errDecoratorCycleDetected struct {
	// Scope in which the cycle was detected.
	scope *Scope

	// Decorator nodes on the stack when the cycle was detected, in the order
	// they were entered.
	stack []*decoratorNode

	// Key that was requested and led back into the stack.
	key key
}

var _ digError = errDecoratorCycleDetected{}

func (e errDecoratorCycleDetected) Error() string {
	b := new(bytes.Buffer)

	if name := e.scope.name; len(name) > 0 {
		fmt.Fprintf(b, "[scope %q]\n", name)
	}

	fmt.Fprintf(b, "cycle detected in decoration chain while building %v:\n", e.key)
	for i, n := range e.stack {
		if i > 0 {
			b.WriteString("\n\tdepends on ")
		}
		fmt.Fprintf(b, "%s", n.stageIdentity())
	}
	if len(e.stack) > 0 {
		b.WriteString("\n\tdepends back on ")
		fmt.Fprintf(b, "%s", e.stack[0].stageIdentity())
	}
	return b.String()
}

func (e errDecoratorCycleDetected) writeMessage(w io.Writer, _ string) {
	fmt.Fprint(w, e.Error())
}

func (e errDecoratorCycleDetected) Format(w fmt.State, c rune) {
	formatError(e, w, c)
}

// decoratorCycleError builds an errDecoratorCycleDetected from the scope's
// current decorator stack. The provided node is the stage that was detected
// as already running; it identifies the entry point of the cycle.
func (s *Scope) decoratorCycleError(n *decoratorNode) error {
	var stack []*decoratorNode
	started := false
	for _, on := range s.decoratorStack {
		if on == n {
			started = true
		}
		if started {
			stack = append(stack, on)
		}
	}
	if len(stack) == 0 {
		stack = append(stack, n)
	}

	key := key{}
	if len(n.resultKeys) > 0 {
		key = n.resultKeys[0]
	}

	return errDecoratorCycleDetected{
		scope: s,
		stack: stack,
		key:   key,
	}
}

// stageStore is a containerStore view used while a decorator stage is being
// built. It makes the stage's own outputs resolve to the previous stage's
// results (or to the nearest decorator in an ancestor Scope), while every
// other type keeps following the normal dependency rules.
//
// All methods that are not explicitly overridden are promoted from the
// embedded Scope, so provider lookup, grouping and value caching behave
// exactly as they do outside of a decorator.
type stageStore struct {
	*Scope

	// The decorator stage currently being built.
	node *decoratorNode
}

var _ containerStore = (*stageStore)(nil)

func newStageStore(s *Scope, n *decoratorNode) *stageStore {
	return &stageStore{Scope: s, node: n}
}

// storesToRoot returns stageStore views of this scope and every ancestor, so
// that a stage's own output in any of those scopes is visible as the
// previous stage's result rather than the output being built.
func (ss *stageStore) storesToRoot() []containerStore {
	scopes := ss.Scope.ancestors()
	stores := make([]containerStore, len(scopes))
	for i, sc := range scopes {
		stores[i] = newStageStore(sc, ss.node)
	}
	return stores
}

// getValueDecorators returns the decorators visible for a named value while
// the stage is building. If the stage itself produces this key, the running
// node is excluded: it receives the previous stage's result instead.
func (ss *stageStore) getValueDecorators(name string, t reflect.Type) []*decoratorNode {
	return ss.previousBindings(ss.Scope.getValueDecorators(name, t), key{name: name, t: t})
}

// getGroupDecorators does the same as getValueDecorators for value groups.
func (ss *stageStore) getGroupDecorators(name string, t reflect.Type) []*decoratorNode {
	return ss.previousBindings(ss.Scope.getGroupDecorators(name, t), key{group: name, t: t})
}

// getValueDecorator returns the nearest previous decorator for the key, or
// false when the stage does not have a predecessor for its own output.
func (ss *stageStore) getValueDecorator(name string, t reflect.Type) (decorator, bool) {
	return ss.lastOf(ss.getValueDecorators(name, t))
}

// getGroupDecorator does the same as getValueDecorator for value groups.
func (ss *stageStore) getGroupDecorator(name string, t reflect.Type) (decorator, bool) {
	return ss.lastOf(ss.getGroupDecorators(name, t))
}

// getDecoratedValue resolves decorated values while the stage is building:
// outputs produced by the running stage are served from the previous stage's
// node-local cache; everything else falls through to the normal lookup.
func (ss *stageStore) getDecoratedValue(name string, t reflect.Type) (reflect.Value, bool) {
	k := key{name: name, t: t}
	if ss.node.hasResultKey(k) {
		if nodes := ss.getValueDecorators(name, t); len(nodes) > 0 {
			if v, ok := ss.getNodeDecoratedValue(nodes[len(nodes)-1], k); ok {
				return v, true
			}
		}
	}
	return ss.Scope.getDecoratedValue(name, t)
}

// getDecoratedValueGroup does the same as getDecoratedValue for value groups.
func (ss *stageStore) getDecoratedValueGroup(name string, t reflect.Type) (reflect.Value, bool) {
	kElem := key{group: name, t: t.Elem()}
	if ss.node.hasResultKey(kElem) {
		if nodes := ss.getGroupDecorators(name, t.Elem()); len(nodes) > 0 {
			if v, ok := ss.getNodeDecoratedValue(nodes[len(nodes)-1], key{group: name, t: t}); ok {
				return v, true
			}
		}
	}
	return ss.Scope.getDecoratedValueGroup(name, t)
}

// previousBindings returns the decorator nodes preceding the running stage
// for the given key. When the stage does not produce the key, the full list
// of bindings is returned unchanged.
func (ss *stageStore) previousBindings(nodes []*decoratorNode, k key) []*decoratorNode {
	if !ss.node.hasResultKey(k) {
		return nodes
	}
	prev := make([]*decoratorNode, 0, len(nodes))
	for _, n := range nodes {
		if n == ss.node {
			break
		}
		prev = append(prev, n)
	}
	return prev
}

func (ss *stageStore) lastOf(nodes []*decoratorNode) (decorator, bool) {
	if len(nodes) == 0 {
		return nil, false
	}
	return nodes[len(nodes)-1], true
}
