package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestReadmeCommandsMatchDispatch keeps cmd/spillway/main.go's dispatch
// switch and the README's Commands table honest with each other (#20).
//
// #11 shipped a CLI command, an admin endpoint, a plugin command and a
// dashboard control, and documented none of them — the gap was found by
// asking, not by anything failing. This covers the CLI slice mechanically:
// every subcommand dispatched in main()'s `switch os.Args[1]` must appear in
// the README's commands table, and the table must not claim a command that
// main() doesn't actually dispatch.
//
// The dispatch switch is found by matching its tag (os.Args[1]) rather than
// scanning every `case "..."` string in the file: parseLevel a little further
// down has `case "debug"/"warn"/"error"` for log levels, and a naive scan
// would demand the README document a log level.
func TestReadmeCommandsMatchDispatch(t *testing.T) {
	groups := dispatchedCommandGroups(t)
	documented := readmeCommandNames(t)

	// Every dispatched command must be documented. A case with several
	// string values (e.g. `case "version", "--version", "-v":`) is one
	// command with aliases, or — as with `case "install", "uninstall":` —
	// a couple of related commands sharing a handler; either way it's
	// satisfied once at least one of its spellings is in the table.
	var undocumented []string
	for _, g := range groups {
		found := false
		for _, name := range g {
			if documented[name] {
				found = true
				break
			}
		}
		if !found {
			undocumented = append(undocumented, strings.Join(g, "/"))
		}
	}
	if len(undocumented) > 0 {
		sort.Strings(undocumented)
		t.Errorf("dispatched but not documented in README's Commands table: %s",
			strings.Join(undocumented, ", "))
	}

	// Every documented command must actually be dispatched.
	dispatched := map[string]bool{}
	for _, g := range groups {
		for _, name := range g {
			dispatched[name] = true
		}
	}
	var undispatched []string
	for name := range documented {
		if !dispatched[name] {
			undispatched = append(undispatched, name)
		}
	}
	if len(undispatched) > 0 {
		sort.Strings(undispatched)
		t.Errorf("documented in README's Commands table but never dispatched: %s",
			strings.Join(undispatched, ", "))
	}
}

// dispatchedCommandGroups parses main.go and returns the string values of
// each non-default case clause of the switch on os.Args[1], one []string
// per clause (so aliases of the same command stay grouped).
func dispatchedCommandGroups(t *testing.T) [][]string {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var mainFunc *ast.FuncDecl
	for _, decl := range file.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "main" {
			mainFunc = fd
			break
		}
	}
	if mainFunc == nil {
		t.Fatal("main.go has no func main()")
	}

	var sw *ast.SwitchStmt
	ast.Inspect(mainFunc.Body, func(n ast.Node) bool {
		if sw != nil {
			return false
		}
		s, ok := n.(*ast.SwitchStmt)
		if !ok {
			return true
		}
		if isOsArgs1(s.Tag) {
			sw = s
			return false
		}
		return true
	})
	if sw == nil {
		t.Fatal("main() has no switch on os.Args[1] — dispatch switch not found; " +
			"has it moved or been renamed?")
	}

	var groups [][]string
	for _, stmt := range sw.Body.List {
		cc, ok := stmt.(*ast.CaseClause)
		if !ok || cc.List == nil { // nil List is the `default:` clause
			continue
		}
		var group []string
		for _, expr := range cc.List {
			lit, ok := expr.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				t.Fatalf("dispatch switch case has non-string-literal expression: %#v", expr)
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatalf("unquote case %s: %v", lit.Value, err)
			}
			group = append(group, v)
		}
		groups = append(groups, group)
	}
	return groups
}

// isOsArgs1 reports whether e is the expression os.Args[1].
func isOsArgs1(e ast.Expr) bool {
	idx, ok := e.(*ast.IndexExpr)
	if !ok {
		return false
	}
	sel, ok := idx.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "os" || sel.Sel.Name != "Args" {
		return false
	}
	lit, ok := idx.Index.(*ast.BasicLit)
	return ok && lit.Kind == token.INT && lit.Value == "1"
}

// readmeCommandRow matches a `| \`spillway <name> ...\` | ... |` row in the
// commands table and captures the command name — the first word after
// "spillway". Names are bare identifiers (switch, login, accounts, ...) so
// \S+ stops cleanly at the next space even when the rest of the row has
// escaped pipes (`accounts overage <name> on\|off\|default`).
var readmeCommandRow = regexp.MustCompile("`spillway ([^\\s`]+)")

// readmeCommandNames reads README.md, isolates the "## Commands" section
// (up to the next "## " heading) and returns the set of command names its
// table documents.
func readmeCommandNames(t *testing.T) map[string]bool {
	t.Helper()

	// The README lives at the repo root; this package is two directories
	// down (cmd/spillway), and `go test` runs with the package directory as
	// its working directory.
	data, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}

	lines := strings.Split(string(data), "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "## Commands" {
			start = i + 1
			break
		}
	}
	if start == -1 {
		t.Fatal("README.md has no \"## Commands\" section")
	}
	end := len(lines)
	for i := start; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") {
			end = i
			break
		}
	}

	names := map[string]bool{}
	for _, line := range lines[start:end] {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		m := readmeCommandRow.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		names[m[1]] = true
	}
	if len(names) == 0 {
		t.Fatal("found the README's Commands section but no `spillway <name>` rows in it")
	}
	return names
}
