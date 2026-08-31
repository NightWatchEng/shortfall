// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

// Command blankline is the deterministic slice of ADR-0014's vertical
// whitespace rule: a blank line is required after the `}` that closes a
// block-bodied statement — `if`, `for`/`range`, `switch`, type switch,
// `select`, with or without a label — when the next line holds more code
// at that level. Four exceptions, and no others: `else`/`else if`
// continuing the same `if`; `case`/`default` opening the next clause; the
// block being the last statement in its list, so what follows is the
// enclosing `}` (the end of a function body included); and a block
// written entirely on one line, which has no closing brace of its own to
// break after. A comment counts as the following code, so the blank line
// goes before it.
//
// The check is an AST walk (go/parser, go/ast, go/token), not a line
// regex: it compares the line of a statement's closing brace against the
// line the next statement in the same list begins on. A multi-line
// composite literal, a wrapped call, a `defer`/`go` with a function
// literal, and a one-line `if err != nil { return err }` all end in a
// brace or paren without being blocks; they are outside the rule and are
// never flagged.
//
// Run `go run ./test/blankline` from the repo root to list violations as
// file:line, and `go run ./test/blankline -fix` to insert the blank lines
// in place; -fix splices a newline textually rather than reprinting the
// file, so the diff is exactly the inserted lines and the result stays
// gofmt-clean. The test in this module runs the same check, which binds
// the convention into the ordinary test step with no gate wiring.
//
// The check excludes nothing: every tracked .go file in the repository is
// parsed, generated files and test files included.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
)

// violation is one place a blank line is required: the file, the line of
// the closing brace it belongs after, and the byte offset at which -fix
// splices the newline (the start of the offending following line).
type violation struct {
	File   string
	Line   int
	Offset int
}

// String renders a violation as file:line, the form every editor and grep
// already understands.
func (v violation) String() string {
	return fmt.Sprintf("%s:%d", v.File, v.Line)
}

// trackedGoFiles asks git for the .go files under root so untracked
// scratch files cannot fail the check.
func trackedGoFiles(root string) ([]string, error) {
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "--", "*.go").Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}

	var files []string
	for _, f := range bytes.Split(out, []byte{0}) {
		if len(f) > 0 {
			files = append(files, filepath.Join(root, string(f)))
		}
	}

	return files, nil
}

// blockBodied reports whether stmt is one of the statements whose body is
// a brace-delimited block on its own lines. A label wraps the statement it
// labels without changing where the block closes, so it is unwrapped
// first; every other statement form — including a `defer`/`go` or an
// assignment whose expression happens to end in `}` — is not a block for
// this rule.
func blockBodied(stmt ast.Stmt) bool {
	for {
		labeled, ok := stmt.(*ast.LabeledStmt)
		if !ok {
			break
		}

		stmt = labeled.Stmt
	}

	switch stmt.(type) {
	case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt,
		*ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt:
		return true
	}

	return false
}

// checker carries the parse of one file: the position table and the
// file's comments flattened and ordered, so the walk can ask what the
// next line of code is without re-scanning.
type checker struct {
	fset     *token.FileSet
	comments []*ast.Comment
	found    []violation
	name     string
}

// firstCommentAfter returns the position of the first comment that starts
// strictly after end and before next, or token.NoPos when there is none.
func (c *checker) firstCommentAfter(end, next token.Pos) token.Pos {
	i := sort.Search(len(c.comments), func(i int) bool {
		return c.comments[i].Pos() > end
	})
	if i < len(c.comments) && c.comments[i].Pos() < next {
		return c.comments[i].Pos()
	}

	return token.NoPos
}

// checkList applies the rule to one statement list: every consecutive
// pair where the first is a multi-line block-bodied statement and the
// second begins on the line immediately after its closing brace. The
// exceptions fall out of the shape rather than needing tests of their
// own: `else` and `case` bodies are not siblings of the block they
// follow, and a block that is last in its list has no successor here.
func (c *checker) checkList(list []ast.Stmt) {
	for i := 0; i+1 < len(list); i++ {
		stmt, next := list[i], list[i+1]
		if !blockBodied(stmt) {
			continue
		}

		open := c.fset.Position(stmt.Pos())
		closing := c.fset.Position(stmt.End())
		if closing.Line == open.Line {
			continue // written on one line: no closing brace of its own
		}

		follows := c.fset.Position(next.Pos())
		if com := c.firstCommentAfter(stmt.End(), next.Pos()); com.IsValid() {
			if p := c.fset.Position(com); p.Line > closing.Line {
				follows = p
			}
		}

		if follows.Line != closing.Line+1 {
			continue
		}

		c.found = append(c.found, violation{
			File:   c.name,
			Line:   closing.Line,
			Offset: follows.Offset - (follows.Column - 1),
		})
	}
}

// check parses src and returns the rule's violations, ordered by position.
func check(name string, src []byte) ([]violation, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", name, err)
	}

	c := &checker{fset: fset, name: name}
	for _, group := range file.Comments {
		c.comments = append(c.comments, group.List...)
	}

	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BlockStmt:
			c.checkList(n.List)
		case *ast.CaseClause:
			c.checkList(n.Body)
		case *ast.CommClause:
			c.checkList(n.Body)
		}

		return true
	})
	sort.Slice(c.found, func(i, j int) bool { return c.found[i].Offset < c.found[j].Offset })
	return c.found, nil
}

// fixed returns src with a blank line spliced in at each violation.
// Splicing runs back to front so earlier offsets stay valid, and inserts
// only a newline, which leaves every other byte of the file — and so
// gofmt's opinion of it — untouched.
func fixed(src []byte, found []violation) []byte {
	out := src
	for i := len(found) - 1; i >= 0; i-- {
		at := found[i].Offset
		spliced := make([]byte, 0, len(out)+1)
		spliced = append(spliced, out[:at]...)
		spliced = append(spliced, '\n')
		spliced = append(spliced, out[at:]...)
		out = spliced
	}

	return out
}

// run checks every tracked .go file under root, fixing in place when fix
// is set, and returns every violation found.
func run(root string, fix bool) ([]violation, error) {
	files, err := trackedGoFiles(root)
	if err != nil {
		return nil, err
	}

	var all []violation
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}

		found, err := check(f, src)
		if err != nil {
			return nil, err
		}

		if len(found) == 0 {
			continue
		}

		all = append(all, found...)
		if fix {
			if err := os.WriteFile(f, fixed(src, found), 0o644); err != nil {
				return nil, err
			}
		}
	}

	return all, nil
}

func main() {
	fix := flag.Bool("fix", false, "insert the missing blank lines in place")
	root := flag.String("root", ".", "repository root to scan")
	flag.Parse()
	found, err := run(*root, *fix)
	if err != nil {
		fmt.Fprintln(os.Stderr, "blankline:", err)
		os.Exit(2)
	}

	files := map[string]bool{}
	for _, v := range found {
		files[v.File] = true
	}

	if *fix {
		fmt.Printf("blankline: inserted %d blank line(s) across %d file(s)\n", len(found), len(files))
		return
	}

	if len(found) > 0 {
		for _, v := range found {
			fmt.Println(v)
		}

		fmt.Fprintf(os.Stderr, "blankline: %d missing blank line(s) after a closing brace in %d file(s) (run: go run ./test/blankline -fix)\n",
			len(found), len(files))
		os.Exit(1)
	}
}
