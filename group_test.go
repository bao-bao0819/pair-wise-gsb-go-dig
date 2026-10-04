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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseGroup(t *testing.T) {
	tests := []struct {
		name    string
		group   string
		wantG   group
		wantErr string
	}{
		{
			name:  "simple group",
			group: `somegroup`,
			wantG: group{Name: "somegroup"},
		},
		{
			name:  "flattened group",
			group: `somegroup,flatten`,
			wantG: group{Name: "somegroup", Flatten: true},
		},
		{
			name:  "soft group",
			group: "somegroup,soft",
			wantG: group{Name: "somegroup", Soft: true},
		},
		{
			name:  "ordered group",
			group: "somegroup,ordered",
			wantG: group{Name: "somegroup", Ordered: true},
		},
		{
			name:  "ordered flatten group",
			group: "somegroup,flatten,ordered",
			wantG: group{Name: "somegroup", Flatten: true, Ordered: true},
		},
		{
			name:    "error",
			group:   `somegroup,abc`,
			wantErr: `invalid option "abc"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotG, err := parseGroupString(tt.group)
			if tt.wantErr != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			assert.Equal(t, tt.wantG, gotG)
		})
	}
}

func TestParseOrderEdges(t *testing.T) {
	t.Run("before", func(t *testing.T) {
		edges, err := parseOrderEdges("auth:audit, recover:*", true)
		require.NoError(t, err)
		assert.Equal(t, []orderEdge{
			{From: "auth", To: "audit"},
			{From: "recover", To: "*"},
		}, edges)
	})

	t.Run("after reverses edge", func(t *testing.T) {
		edges, err := parseOrderEdges("audit:auth", false)
		require.NoError(t, err)
		assert.Equal(t, []orderEdge{{From: "auth", To: "audit"}}, edges)
	})

	for _, bad := range []string{"auth", "a:b:c", ":b"} {
		_, err := parseOrderEdges(bad, true)
		assert.Error(t, err, bad)
	}
}
