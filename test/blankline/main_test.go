// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"go/format"
	"strings"
	"testing"
)

// preambleLines is how many lines wrap returns ahead of a case body, so a
// case can state the body-relative line it expects to be flagged.
const preambleLines = 3

// wrap turns a case body into a compilable file. The body is written with
// a leading newline for readability and one tab of indentation, which is
// what gofmt would produce, so the fixed forms can be asserted to be
// gofmt-clean.
func wrap(body string) string {
	return "package a\n\nfunc f() {\n" + strings.TrimPrefix(body, "\n") + "}\n"
}

// lines reports the body-relative lines the check flagged.
func lines(found []violation) []int {
	var got []int
	for _, v := range found {
		got = append(got, v.Line-preambleLines)
	}

	return got
}

func equal(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

// TestCheck pins the rule itself: which shapes are violations, which are
// the four exceptions, and which statement forms are not blocks at all.
// want holds the body-relative lines of the closing braces that must be
// flagged; nil means the body is compliant.
func TestCheck(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []int
	}{
		{
			name: "plain violation",
			body: `
	if x {
		a()
	}
	b()
`,
			want: []int{3},
		},
		{
			name: "compliant pair",
			body: `
	if x {
		a()
	}

	b()
`,
		},
		{
			name: "else chain is one statement",
			body: `
	if x {
		a()
	} else if y {
		b()
	} else {
		c()
	}

	d()
`,
		},
		{
			name: "else chain still needs the break after its last brace",
			body: `
	if x {
		a()
	} else if y {
		b()
	} else {
		c()
	}
	d()
`,
			want: []int{7},
		},
		{
			name: "case and default open no violation",
			body: `
	switch x {
	case 1:
		if y {
			a()
		}
	case 2:
		b()
	default:
		c()
	}

	d()
`,
		},
		{
			name: "block inside a case body has a successor",
			body: `
	switch x {
	case 1:
		if y {
			a()
		}
		b()
	}

	c()
`,
			want: []int{5},
		},
		{
			name: "block is the last statement in the function body",
			body: `
	if x {
		a()
	}
`,
		},
		{
			name: "one-line if has no closing brace of its own",
			body: `
	if err != nil { return err }
	b()
`,
		},
		{
			name: "block followed by a closing brace",
			body: `
	for i := range n {
		if x {
			a()
		}
	}

	b()
`,
		},
		{
			name: "nested block with a successor",
			body: `
	for i := range n {
		if x {
			a()
		}
		b()
	}

	c()
`,
			want: []int{4},
		},
		{
			name: "three-clause for",
			body: `
	for i := 0; i < n; i++ {
		a()
	}
	b()
`,
			want: []int{3},
		},
		{
			name: "switch",
			body: `
	switch x {
	case 1:
		a()
	}
	b()
`,
			want: []int{4},
		},
		{
			name: "type switch",
			body: `
	switch v := x.(type) {
	case int:
		_ = v
	}
	b()
`,
			want: []int{4},
		},
		{
			name: "select",
			body: `
	select {
	case <-ch:
		a()
	}
	b()
`,
			want: []int{4},
		},
		{
			name: "block inside a select comm clause has a successor",
			body: `
	select {
	case <-ch:
		if x {
			a()
		}
		b()
	}

	c()
`,
			want: []int{5},
		},
		{
			name: "labelled loop",
			body: `
outer:
	for {
		break outer
	}
	b()
`,
			want: []int{4},
		},
		{
			name: "defer with a function literal is not a block",
			body: `
	defer func() {
		a()
	}()
	b()
`,
		},
		{
			name: "go with a function literal is not a block",
			body: `
	go func() {
		a()
	}()
	b()
`,
		},
		{
			name: "composite literal is not a block",
			body: `
	v := []int{
		1,
	}
	b()
`,
		},
		{
			name: "block inside a function literal passed as an argument",
			body: `
	run(func() {
		if x {
			a()
		}
		b()
	})

	c()
`,
			want: []int{4},
		},
		{
			name: "comment on the next line counts as the next code",
			body: `
	if x {
		a()
	}
	// why b matters
	b()
`,
			want: []int{3},
		},
		{
			name: "comment after a blank line is compliant",
			body: `
	if x {
		a()
	}

	// why b matters
	b()
`,
		},
		{
			name: "trailing comment on the brace line is not the break",
			body: `
	if x {
		a()
	} // done
	b()
`,
			want: []int{3},
		},
		{
			name: "two violations in one body",
			body: `
	if x {
		a()
	}
	for range n {
		b()
	}
	c()
`,
			want: []int{3, 6},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			found, err := check("case.go", []byte(wrap(tc.body)))
			if err != nil {
				t.Fatalf("check: %v", err)
			}

			if got := lines(found); !equal(got, tc.want) {
				t.Errorf("flagged body lines %v, want %v", got, tc.want)
			}
		})
	}
}

// TestFixed pins what -fix writes: the blank line lands in the right
// place, the result no longer violates the rule, and it is gofmt-clean —
// the property that lets the fix splice text instead of reprinting the
// file.
func TestFixed(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "plain violation",
			body: `
	if x {
		a()
	}
	b()
`,
			want: `
	if x {
		a()
	}

	b()
`,
		},
		{
			name: "blank line goes before the comment",
			body: `
	if x {
		a()
	}
	// why b matters
	b()
`,
			want: `
	if x {
		a()
	}

	// why b matters
	b()
`,
		},
		{
			name: "two violations in one body",
			body: `
	if x {
		a()
	}
	for range n {
		b()
	}
	c()
`,
			want: `
	if x {
		a()
	}

	for range n {
		b()
	}

	c()
`,
		},
		{
			name: "nested violation keeps the enclosing indentation",
			body: `
	for range n {
		if x {
			a()
		}
		b()
	}
`,
			want: `
	for range n {
		if x {
			a()
		}

		b()
	}
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := []byte(wrap(tc.body))
			found, err := check("case.go", src)
			if err != nil {
				t.Fatalf("check: %v", err)
			}

			got := fixed(src, found)
			if want := wrap(tc.want); string(got) != want {
				t.Errorf("fixed:\n%s\nwant:\n%s", got, want)
			}

			again, err := check("case.go", got)
			if err != nil {
				t.Fatalf("re-check: %v", err)
			}

			if len(again) > 0 {
				t.Errorf("fixed content still reports %d violation(s)", len(again))
			}

			formatted, err := format.Source(got)
			if err != nil {
				t.Fatalf("gofmt: %v", err)
			}

			if string(formatted) != string(got) {
				t.Errorf("fixed content is not gofmt-clean:\n%s", formatted)
			}
		})
	}
}

// TestEveryTrackedGoFileBreaksAfterItsBlocks is the durable guard: a
// closing brace with a statement on the very next line fails the ordinary
// test step, naming the file, the line, and the fix command.
func TestEveryTrackedGoFileBreaksAfterItsBlocks(t *testing.T) {
	found, err := run("../..", false)
	if err != nil {
		t.Fatalf("scanning tracked files: %v", err)
	}

	if len(found) > 0 {
		var at []string
		for _, v := range found {
			at = append(at, v.String())
		}

		t.Errorf("%d missing blank line(s) after a closing brace (run: go run ./test/blankline -fix):\n%s",
			len(found), strings.Join(at, "\n"))
	}
}
