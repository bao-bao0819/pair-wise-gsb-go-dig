// Copyright (c) 2020 Uber Technologies, Inc.
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
	"strings"
)

const (
	_groupTag = "group"

	// Tag applied to a value-group producer that names the member (or the
	// contiguous block produced via flatten) so that ordered consumers can
	// reference it in before/after constraints.
	_groupTagTag = "grouptag"

	// Tags applied to an ordered value-group consumer that declare ordering
	// constraints between members. Each comma-separated entry has the form
	// "from:before" naming two member tags; the member tagged "from" will be
	// placed before (or after) the member tagged "before".
	_groupBeforeTag = "groupbefore"
	_groupAfterTag  = "groupafter"

	// Wildcard used in before/after constraints to refer to every member,
	// including members that do not declare a tag.
	_orderWildcard = "*"
)

type group struct {
	Name    string
	Flatten bool
	Soft    bool
	// Ordered is set on value-group consumers that require the collected
	// slice to honor member tags and before/after constraints. It is invalid
	// on producers.
	Ordered bool
}

// orderEdge is a single ordering constraint between two member tags.
type orderEdge struct {
	From string
	To   string
}

type errInvalidGroupOption struct{ Option string }

var _ digError = errInvalidGroupOption{}

func (e errInvalidGroupOption) Error() string { return fmt.Sprint(e) }

func (e errInvalidGroupOption) writeMessage(w io.Writer, v string) {
	fmt.Fprintf(w, "invalid option %q", e.Option)
}

func (e errInvalidGroupOption) Format(w fmt.State, c rune) {
	formatError(e, w, c)
}

func parseGroupString(s string) (group, error) {
	components := strings.Split(s, ",")
	g := group{Name: components[0]}
	for _, c := range components[1:] {
		switch c {
		case "flatten":
			g.Flatten = true
		case "soft":
			g.Soft = true
		case "ordered":
			g.Ordered = true
		default:
			return g, errInvalidGroupOption{Option: c}
		}
	}
	return g, nil
}

// validateGroupTag validates a member tag declared on a value-group
// producer. Tags must be non-empty and may not contain runes used to parse
// group tags or ordering constraints.
func validateGroupTag(tag string) error {
	switch {
	case tag == "":
		return nil
	case strings.ContainsAny(tag, ",:*`"):
		return newErrInvalidInput(
			fmt.Sprintf("invalid value group member tag %q: tags may not contain commas, colons, stars or backquotes", tag), nil)
	}
	return nil
}

// parseOrderEdges parses the value of a groupbefore/groupafter struct tag.
//
// Entries are comma-separated "from:to" pairs. For a "before" tag each entry
// means "from before to"; for an "after" tag it means "from after to" (the
// edge is reversed).
func parseOrderEdges(tagValue string, before bool) ([]orderEdge, error) {
	var edges []orderEdge
	for _, entry := range strings.Split(tagValue, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.Split(entry, ":")
		if len(parts) != 2 {
			return nil, newErrInvalidInput(
				fmt.Sprintf("invalid ordering constraint %q: expected \"from:to\"", entry), nil)
		}
		from, to := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if !validOrderTag(from) || !validOrderTag(to) {
			return nil, newErrInvalidInput(
				fmt.Sprintf("invalid ordering constraint %q: tag names must be non-empty and may not contain commas, colons or backquotes", entry), nil)
		}
		if !before {
			from, to = to, from
		}
		edges = append(edges, orderEdge{From: from, To: to})
	}
	return edges, nil
}

func validOrderTag(tag string) bool {
	return tag != "" && !strings.ContainsAny(tag, ":`") && !strings.Contains(tag, ",")
}
