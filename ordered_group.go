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
)

// orderedGroupMember is one contiguous block contributed to an ordered value
// group. A flattened slice, or a constructor returning multiple grouped
// results, is represented as a separate member per result.
type orderedGroupMember struct {
	// node that produces this member.
	node *constructorNode

	// Scope the producing constructor was provided to.
	scope *Scope

	// Description of the result that produces this member, in the same shape
	// as the producing constructor's resultList tree.
	result resultGrouped

	// block is the position of this member's result among its constructor's
	// matching results for the consumed element type. It identifies the block
	// captured from the constructor.
	block int

	// Position of the member's producing node in the effective Scope's graph.
	// Nodes created before a child Scope was opened share this order with
	// their copies in the child.
	order int

	// seq is a stable tiebreaker: it increases in the order in which members
	// were collected (child scopes first, then ancestors).
	seq int
}

func (m orderedGroupMember) label() string { return m.result.Label }

// memberName identifies a member in user-facing errors.
func (m orderedGroupMember) memberName() string {
	if m.result.Label != "" {
		return fmt.Sprintf("member %q", m.result.Label)
	}
	return fmt.Sprintf("unlabeled member from %v", m.node.Location())
}

// collectOrderedGroupMembers enumerates all members visible to the given
// scope that contribute values to the group consumed by pt.
func collectOrderedGroupMembers(pt paramGroupedSlice, c containerStore) []orderedGroupMember {
	var members []orderedGroupMember
	seq := 0
	for _, store := range c.storesToRoot() {
		s := store.(*Scope)
		providers := s.getGroupProviders(pt.Group, pt.Type.Elem())
		for _, p := range providers {
			n, ok := p.(*constructorNode)
			if !ok {
				continue
			}
			results := orderedResultsFor(n, pt.Group, pt.Type.Elem())
			for block, r := range results {
				members = append(members, orderedGroupMember{
					node:   n,
					scope:  n.OrigScope(),
					result: r,
					block:  block,
					order:  n.Order(s),
					seq:    seq,
				})
				seq++
			}
		}
	}
	return members
}

// orderedResultsFor returns the resultGrouped results produced by n for the
// requested group and element type, in the order in which the results are
// extracted.
func orderedResultsFor(n *constructorNode, group string, elem reflect.Type) []resultGrouped {
	var matches []resultGrouped
	var walk func(r result)
	walk = func(r result) {
		switch res := r.(type) {
		case resultGrouped:
			if res.Group == group && resultGroupProduces(res, elem) {
				matches = append(matches, res)
			}
		case resultObject:
			for _, f := range res.Fields {
				walk(f.Result)
			}
		case resultList:
			for _, child := range res.Results {
				walk(child)
			}
		}
	}
	walk(n.resultList)
	return matches
}

// resultGroupProduces reports whether the grouped result contributes values
// of the requested element type, including types added with dig.As.
func resultGroupProduces(r resultGrouped, elem reflect.Type) bool {
	if r.Type == elem {
		return true
	}
	for _, asType := range r.As {
		if asType == elem {
			return true
		}
	}
	return false
}

// orderOrderedGroupMembers validates member labels and constraints and returns
// the members sorted into their final, stable order. Constraints reference
// members by label rather than function identity.
func orderOrderedGroupMembers(pt paramGroupedSlice, members []orderedGroupMember) ([]orderedGroupMember, error) {
	byLabel := make(map[string]orderedGroupMember)
	for _, m := range members {
		if m.label() == "" {
			continue
		}
		if existing, ok := byLabel[m.label()]; ok {
			return nil, newErrInvalidInput(
				fmt.Sprintf("duplicate label %q in value group %v", m.label(), pt.Type.Elem()),
				newErrInvalidInput(fmt.Sprintf(
					"label used by both %s and %s; labels must be unique across the scope and its ancestors",
					existing.memberName(), m.memberName()), nil))
		}
		byLabel[m.label()] = m
	}

	// edges[u] is the set of member indices that must come after member u.
	edges := make([]map[int]struct{}, len(members))
	indegree := make([]int, len(members))
	for i := range members {
		edges[i] = make(map[int]struct{})
	}

	type memberID struct {
		node  *constructorNode
		block int
	}
	memberIndex := make(map[memberID]int, len(members))
	for i, m := range members {
		memberIndex[memberID{m.node, m.block}] = i
	}

	addEdge := func(from, to int) {
		if _, ok := edges[from][to]; ok {
			return
		}
		edges[from][to] = struct{}{}
		indegree[to]++
	}

	for _, m := range members {
		from := memberIndex[memberID{m.node, m.block}]
		for _, label := range m.result.Before {
			target, ok := byLabel[label]
			if !ok {
				return nil, newErrInvalidInput(
					fmt.Sprintf("unknown before label %q in value group %v", label, pt.Type.Elem()),
					newErrInvalidInput(fmt.Sprintf("referenced by %s", m.memberName()), nil))
			}
			addEdge(from, memberIndex[memberID{target.node, target.block}])
		}
		for _, label := range m.result.After {
			target, ok := byLabel[label]
			if !ok {
				return nil, newErrInvalidInput(
					fmt.Sprintf("unknown after label %q in value group %v", label, pt.Type.Elem()),
					newErrInvalidInput(fmt.Sprintf("referenced by %s", m.memberName()), nil))
			}
			addEdge(memberIndex[memberID{target.node, target.block}], from)
		}
	}

	// Fail fast on cycles before any propagation or sorting.
	if stuck := detectConstraintCycle(edges, indegree); len(stuck) > 0 {
		sort.Slice(stuck, func(a, b int) bool {
			ma, mb := members[stuck[a]], members[stuck[b]]
			if ma.order != mb.order {
				return ma.order < mb.order
			}
			return ma.seq < mb.seq
		})
		names := make([]string, 0, len(stuck))
		for _, i := range stuck {
			names = append(names, members[i].memberName())
		}
		return nil, newErrInvalidInput(
			fmt.Sprintf("cycle detected in ordered value group %v", pt.Type.Elem()),
			newErrInvalidInput(strings.Join(names, "; "), nil))
	}

	n := len(members)
	successors := make([][]int, n)
	for u, vs := range edges {
		for v := range vs {
			successors[u] = append(successors[u], v)
		}
	}

	// Registration order, which is the stable order of members that have no
	// constraint relationship. Nodes provided before a child Scope was opened
	// share their graph order across the scope chain.
	stable := make([]int, n)
	for i := range stable {
		stable[i] = i
	}
	sort.SliceStable(stable, func(a, b int) bool {
		mi, mj := members[stable[a]], members[stable[b]]
		if mi.order != mj.order {
			return mi.order < mj.order
		}
		return mi.seq < mj.seq
	})
	stableRank := make([]int, n)
	for rank, i := range stable {
		stableRank[i] = rank
	}

	// Kahn's algorithm with a stable priority: emit the available member
	// with the earliest registration position. This is a linear extension of
	// the constraint partial order that preserves the relative registration
	// order of members that are unrelated by constraints.
	remaining := append([]int(nil), indegree...)
	available := make([]int, 0, n)
	for _, i := range stable {
		if remaining[i] == 0 {
			available = append(available, i)
		}
	}
	popBest := func() int {
		best := 0
		for k := 1; k < len(available); k++ {
			if stableRank[available[k]] < stableRank[available[best]] {
				best = k
			}
		}
		v := available[best]
		available = append(available[:best], available[best+1:]...)
		return v
	}

	result := make([]int, 0, n)
	for len(available) > 0 {
		u := popBest()
		result = append(result, u)
		for _, v := range successors[u] {
			remaining[v]--
			if remaining[v] == 0 {
				available = append(available, v)
			}
		}
	}
	if len(result) != n {
		// Cycles are reported earlier; this is a defensive safeguard.
		return nil, newErrInvalidInput(
			fmt.Sprintf("cycle detected in ordered value group %v", pt.Type.Elem()), nil)
	}

	sorted := make([]orderedGroupMember, 0, n)
	for _, i := range result {
		sorted = append(sorted, members[i])
	}
	return sorted, nil
}

// detectConstraintCycle returns the member indices participating in a
// constraint cycle, or nil if the graph is acyclic.
func detectConstraintCycle(edges []map[int]struct{}, indegree []int) []int {
	indeg := append([]int(nil), indegree...)
	var queue []int
	for i, d := range indeg {
		if d == 0 {
			queue = append(queue, i)
		}
	}
	visited := 0
	for len(queue) > 0 {
		u := queue[0]
		queue = queue[1:]
		visited++
		for v := range edges[u] {
			indeg[v]--
			if indeg[v] == 0 {
				queue = append(queue, v)
			}
		}
	}
	if visited == len(indeg) {
		return nil
	}
	var stuck []int
	for i, d := range indeg {
		if d > 0 {
			stuck = append(stuck, i)
		}
	}
	return stuck
}
