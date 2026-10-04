// Copyright (c) 2026 Uber Technologies, Inc.
//
// Licensed under the same terms as the rest of the dig project: see the
// MIT license text in accompanying source files.

package dig

import (
	"fmt"
	"io"
	"reflect"
)

// orderedGroupBlock is one unit considered when sorting an ordered value
// group: a single non-flattened member, or the whole (possibly empty)
// contiguous slice produced by one flatten result.
type orderedGroupBlock struct {
	// Tag declared by the producer. Empty for untagged members.
	Tag string

	// Registration order within the effective scope chain. Lower values
	// were registered earlier. Used as the stable tie-breaker for blocks
	// that are not related by ordering constraints.
	Registration int

	// Producer of this block.
	node *constructorNode

	// seq is the constructor-wide index of the originating grouped result,
	// used to align the plan with values captured after the producer runs.
	seq int

	// Values produced for the block. Empty for a flatten constructor that
	// returned an empty slice.
	values []reflect.Value

	// emitted is used during topological sorting.
	emitted bool
}

// orderConflictError reports two members that declared the same tag in
// scopes visible to an ordered consumer.
type orderConflictError struct {
	Group string
	Tag   string
	First *constructorNode
	Other *constructorNode
}

var _ digError = orderConflictError{}

func (e orderConflictError) Error() string { return fmt.Sprint(e) }

func (e orderConflictError) writeMessage(w io.Writer, _ string) {
	fmt.Fprintf(w,
		"duplicate member tag %q in value group %q: provided by %v and %v",
		e.Tag, e.Group, e.First.Location(), e.Other.Location())
}

func (e orderConflictError) Format(w fmt.State, c rune) { formatError(e, w, c) }

// orderUnknownTagError reports a before/after constraint referencing a tag
// that no visible member declared.
type orderUnknownTagError struct {
	Group string
	Tag   string
}

var _ digError = orderUnknownTagError{}

func (e orderUnknownTagError) Error() string { return fmt.Sprint(e) }

func (e orderUnknownTagError) writeMessage(w io.Writer, _ string) {
	fmt.Fprintf(w, "unknown member tag %q referenced by an ordering constraint in value group %q",
		e.Tag, e.Group)
}

func (e orderUnknownTagError) Format(w fmt.State, c rune) { formatError(e, w, c) }

// orderCycleError reports a directed cycle among ordering constraints.
type orderCycleError struct {
	Group string
	// Tags participating in the cycle, in the order in which they were
	// reached. Untagged members are described by their constructor type.
	Tags []string
}

var _ digError = orderCycleError{}

func (e orderCycleError) Error() string { return fmt.Sprint(e) }

func (e orderCycleError) writeMessage(w io.Writer, _ string) {
	fmt.Fprintf(w, "cycle detected in ordering constraints for value group %q: %s",
		e.Group, joinCycleTags(e.Tags))
}

func (e orderCycleError) Format(w fmt.State, c rune) { formatError(e, w, c) }

func joinCycleTags(tags []string) string {
	if len(tags) == 0 {
		return ""
	}
	out := tags[0]
	for _, t := range tags[1:] {
		out += " -> " + t
	}
	out += " -> " + tags[0]
	return out
}

// planOrderedGroup builds the static, provider-derived block list for the
// given group key. Blocks are ordered by registration order across the
// effective scope chain (root scope first, then descending scopes).
//
// This inspects provider result definitions only and never invokes a
// constructor.
func planOrderedGroup(k key, c containerStore) []orderedGroupBlock {
	stores := c.storesToRoot()
	// storesToRoot returns the current scope first; registration order across
	// scopes is read root-first.
	var blocks []orderedGroupBlock
	registration := 0
	for i := len(stores) - 1; i >= 0; i-- {
		s, ok := stores[i].(*Scope)
		if !ok {
			continue
		}
		for _, n := range s.providers[k] {
			for _, contrib := range groupContributionsFor(n, k) {
				blocks = append(blocks, orderedGroupBlock{
					Tag:          contrib.tag,
					Registration: registration,
					node:         n,
					seq:          contrib.index,
				})
				registration++
			}
		}
	}
	return blocks
}

// groupContribution pairs the node-wide index of a grouped result with the
// tag it carries. The index counts every resultGrouped result that targets
// the group (regardless of element type), which matches the order in which
// blocks are captured after the constructor runs.
type groupContribution struct {
	index int
	tag   string
}

// groupContributionsFor returns the contributions of a constructor to one
// group/element-type key, in result-extraction order. A flatten result
// contributes exactly one block (it may expand to zero or many values).
func groupContributionsFor(n *constructorNode, k key) []groupContribution {
	var contribs []groupContribution
	index := 0
	walkResult(n.resultList, groupTagCollector{key: k, index: &index, contribs: &contribs})
	return contribs
}

type groupTagCollector struct {
	key      key
	index    *int
	contribs *[]groupContribution
}

func (v groupTagCollector) Visit(r result) resultVisitor {
	if rg, ok := r.(resultGrouped); ok &&
		rg.Group == v.key.group {
		idx := *v.index
		*v.index++
		if rg.Type == v.key.t || containsType(rg.As, v.key.t) {
			*v.contribs = append(*v.contribs, groupContribution{index: idx, tag: rg.Tag})
		}
	}
	return v
}

func (v groupTagCollector) AnnotateWithField(resultObjectField) resultVisitor { return v }
func (v groupTagCollector) AnnotateWithPosition(int) resultVisitor            { return v }

func containsType(types []reflect.Type, t reflect.Type) bool {
	for _, typ := range types {
		if typ == t {
			return true
		}
	}
	return false
}

// fillOrderedBlockValues populates each block from its producer's captured
// results. Producers that have not been called contribute no values (this
// happens for soft groups).
func fillOrderedBlockValues(blocks []orderedGroupBlock, k key) {
	for i := range blocks {
		b := &blocks[i]
		if values, ok := findBlockValues(b.node, k, b.seq); ok {
			b.values = values
		}
	}
}

// findBlockValues returns the runtime values for the grouped result with the
// given seq. For element types introduced via dig.As, the values are stored
// under the result's primary element type, so that result is consulted too.
func findBlockValues(n *constructorNode, k key, seq int) ([]reflect.Value, bool) {
	if produced, ok := n.groupResults[k]; ok {
		for _, p := range produced {
			if p.Seq == seq {
				return p.Values, true
			}
		}
	}

	var primary reflect.Type
	walkResult(n.resultList, primaryGroupFinder{key: k, out: &primary})
	if primary == nil {
		return nil, false
	}
	pk := key{group: k.group, t: primary}
	for _, p := range n.groupResults[pk] {
		if p.Seq == seq {
			return p.Values, true
		}
	}
	return nil, false
}

type primaryGroupFinder struct {
	key key
	out *reflect.Type
}

func (v primaryGroupFinder) Visit(r result) resultVisitor {
	if rg, ok := r.(resultGrouped); ok &&
		rg.Group == v.key.group && containsType(rg.As, v.key.t) {
		*v.out = rg.Type
		return nil
	}
	return v
}

func (v primaryGroupFinder) AnnotateWithField(resultObjectField) resultVisitor { return v }
func (v primaryGroupFinder) AnnotateWithPosition(int) resultVisitor            { return v }

// sortOrderedBlocks validates constraints and returns blocks in append order.
// Constraints are resolved by member tag rather than constructor identity;
// registration order is the stable tie-breaker.
func sortOrderedBlocks(groupName string, blocks []orderedGroupBlock, edges []orderEdge) ([]orderedGroupBlock, error) {
	n := len(blocks)

	// Duplicate tags among visible members conflict. Untagged members never
	// conflict.
	tagOwner := make(map[string]int)
	for i, b := range blocks {
		if b.Tag == "" {
			continue
		}
		if first, ok := tagOwner[b.Tag]; ok {
			return nil, orderConflictError{
				Group: groupName,
				Tag:   b.Tag,
				First: blocks[first].node,
				Other: blocks[i].node,
			}
		}
		tagOwner[b.Tag] = i
	}

	// Validate referenced tags before expanding edges.
	for _, e := range edges {
		if e.From != _orderWildcard {
			if _, ok := tagOwner[e.From]; !ok {
				return nil, orderUnknownTagError{Group: groupName, Tag: e.From}
			}
		}
		if e.To != _orderWildcard {
			if _, ok := tagOwner[e.To]; !ok {
				return nil, orderUnknownTagError{Group: groupName, Tag: e.To}
			}
		}
	}

	indegree := make([]int, n)
	adj := make([][]int, n)
	addEdge := func(from, to int) {
		adj[from] = append(adj[from], to)
		indegree[to]++
	}

	all := make([]int, n)
	for i := range all {
		all[i] = i
	}
	resolve := func(tag string) []int {
		if tag == _orderWildcard {
			return all
		}
		return []int{tagOwner[tag]}
	}
	for _, e := range edges {
		for _, f := range resolve(e.From) {
			for _, t := range resolve(e.To) {
				if f == t {
					// A wildcard self-match is vacuous.
					continue
				}
				addEdge(f, t)
			}
		}
	}

	// Kahn's algorithm. Blocks are already in registration order, so picking
	// the lowest-index ready block each round yields a stable topological
	// ordering: related members follow the constraints while unrelated
	// members keep registration order.
	emitted := make([]orderedGroupBlock, 0, n)
	for len(emitted) < n {
		picked := -1
		for i := 0; i < n; i++ {
			if indegree[i] == 0 && !blocks[i].emitted {
				picked = i
				break
			}
		}
		if picked == -1 {
			return nil, orderCycleError{Group: groupName, Tags: cyclicTags(blocks, indegree)}
		}
		blocks[picked].emitted = true
		emitted = append(emitted, blocks[picked])
		for _, t := range adj[picked] {
			indegree[t]--
		}
	}
	return emitted, nil
}

func cyclicTags(blocks []orderedGroupBlock, indegree []int) []string {
	var tags []string
	for i := range blocks {
		if indegree[i] > 0 {
			tag := blocks[i].Tag
			if tag == "" {
				tag = fmt.Sprintf("<%v>", blocks[i].node.location)
			}
			tags = append(tags, tag)
		}
	}
	return tags
}
