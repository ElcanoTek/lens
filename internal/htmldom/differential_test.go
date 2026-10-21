// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package htmldom

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// testdata/differential.json holds random markup built from tokenizer-hostile
// pieces (unclosed quotes, stray end tags, broken references, RAWTEXT and
// RCDATA elements …) with the tree, text and markup the Python html_dom
// module produced for each on CPython 3.12. The Go parser must agree on
// every one.

type diffCase struct {
	In    string `json:"in"`
	Tree  any    `json:"tree"`
	Text  string `json:"text"`
	Strip string `json:"strip"`
	Str   string `json:"str"`
}

// dump renders n the way the corpus generator did: elements as
// [name, [[key, value]...], [children...]], strings as {"k": kind, "d": data}.
func dump(n *Node) any {
	if n.Type == TextNode {
		return map[string]any{"k": string(n.Kind), "d": n.Data}
	}
	attrs := []any{}
	for _, a := range n.Attrs {
		attrs = append(attrs, []any{a.Key, a.Val})
	}
	children := []any{}
	for _, c := range n.Children() {
		children = append(children, dump(c))
	}
	return []any{n.Name, attrs, children}
}

func TestDifferentialAgainstPython(t *testing.T) {
	raw, err := os.ReadFile("testdata/differential.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []diffCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	failures := 0
	for _, c := range cases {
		doc := Parse(c.In)
		// Round-trip through JSON so both sides have the same dynamic types.
		var got any
		encoded, _ := json.Marshal(dump(doc))
		_ = json.Unmarshal(encoded, &got)
		if !reflect.DeepEqual(got, c.Tree) || doc.Text() != c.Text ||
			doc.GetText(" ", true) != c.Strip || doc.String() != c.Str {
			failures++
			if failures <= 5 {
				t.Errorf("input %q\n got  %s\n want %v\n text %q / %q\n str %q / %q",
					c.In, encoded, c.Tree, doc.Text(), c.Text, doc.String(), c.Str)
			}
		}
	}
	if failures > 0 {
		t.Errorf("%d of %d cases differ", failures, len(cases))
	}
}
