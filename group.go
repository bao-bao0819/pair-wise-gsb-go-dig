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
)

type group struct {
	Name    string
	Flatten bool
	Soft    bool

	// Ordered reports that the group was consumed with the "ordered"
	// option. Only grouped values consumed this way honor Label, Before and
	// After; it is rejected on producers.
	Ordered bool

	// Label optionally names the member contributed by a grouped result so
	// that other members can refer to it from Before or After.
	Label string

	// Before and After constrain the position of this member relative to the
	// members carrying the referenced labels. They are resolved by label
	// rather than the producing function.
	Before []string
	After  []string
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

// errInvalidGroupTagValue is returned when a group option is recognized but
// its value is malformed.
type errInvalidGroupTagValue struct {
	Option string
	Value  string
	Reason string
}

var _ digError = errInvalidGroupTagValue{}

func (e errInvalidGroupTagValue) Error() string { return fmt.Sprint(e) }

func (e errInvalidGroupTagValue) Unwrap() error { return nil }

func (e errInvalidGroupTagValue) writeMessage(w io.Writer, _ string) {
	fmt.Fprintf(w, "invalid value %q for group option %q: %s", e.Value, e.Option, e.Reason)
}

func (e errInvalidGroupTagValue) Format(w fmt.State, c rune) {
	formatError(e, w, c)
}

func parseGroupString(s string) (group, error) {
	components := strings.Split(s, ",")
	g := group{Name: components[0]}
	for _, c := range components[1:] {
		opt, value, hasValue := strings.Cut(c, "=")
		switch opt {
		case "flatten", "soft", "ordered":
			if hasValue {
				return g, errInvalidGroupOption{Option: c}
			}
			switch opt {
			case "flatten":
				g.Flatten = true
			case "soft":
				g.Soft = true
			case "ordered":
				g.Ordered = true
			}
		case "label":
			if !hasValue {
				return g, errInvalidGroupTagValue{
					Option: opt,
					Reason: `expected label="<name>"`,
				}
			}
			if err := validateGroupLabel(opt, value); err != nil {
				return g, err
			}
			g.Label = value
		case "before", "after":
			if !hasValue {
				return g, errInvalidGroupTagValue{
					Option: opt,
					Reason: fmt.Sprintf(`expected %s="<label>[;<label>...]"`, opt),
				}
			}
			labels, err := parseGroupLabelList(opt, value)
			if err != nil {
				return g, err
			}
			if opt == "before" {
				g.Before = append(g.Before, labels...)
			} else {
				g.After = append(g.After, labels...)
			}
		default:
			return g, errInvalidGroupOption{Option: c}
		}
	}
	return g, nil
}

// validateGroupLabel verifies that a single member label is usable inside a
// group tag.
func validateGroupLabel(option, label string) error {
	if label == "" {
		return errInvalidGroupTagValue{
			Option: option,
			Value:  label,
			Reason: "label must not be empty",
		}
	}
	if strings.ContainsAny(label, ",;=") {
		return errInvalidGroupTagValue{
			Option: option,
			Value:  label,
			Reason: "label must not contain ',', ';' or '='",
		}
	}
	return nil
}

// parseGroupLabelList parses a ';'-separated list of member labels used by
// the before/after group options.
func parseGroupLabelList(option, list string) ([]string, error) {
	var labels []string
	seen := make(map[string]struct{})
	for _, label := range strings.Split(list, ";") {
		if err := validateGroupLabel(option, label); err != nil {
			return nil, err
		}
		if _, ok := seen[label]; ok {
			continue
		}
		seen[label] = struct{}{}
		labels = append(labels, label)
	}
	if len(labels) == 0 {
		return nil, errInvalidGroupTagValue{
			Option: option,
			Value:  list,
			Reason: "at least one label is required",
		}
	}
	return labels, nil
}
