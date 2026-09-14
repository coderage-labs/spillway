package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"

	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/logfile"
)

// Under a service manager stderr is a file the daemon did not open. It is
// the one file spillway can never rotate, so nothing may be copied there:
// a second transcript of an already-bounded log, growing forever, is issue
// #171 all over again — and that is how the original 236 MB was written.
func TestServiceManagerStderrGetsNoCopyOfTheLog(t *testing.T) {
	dir := t.TempDir()

	// Exactly what launchd hands the process: a plain file on fd 2.
	redirected, err := os.Create(filepath.Join(dir, "stderr.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer redirected.Close()

	lf, err := logfile.Open(filepath.Join(dir, "spillway.log"), 1<<20, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()

	out := daemonLogOut(lf, redirected)
	if _, err := out.Write([]byte("level=INFO msg=request\n")); err != nil {
		t.Fatal(err)
	}

	stderrBytes, err := os.ReadFile(filepath.Join(dir, "stderr.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(stderrBytes) != 0 {
		t.Errorf("a redirected stderr received %q; under launchd or the Scheduled Task "+
			"that file is unbounded and unrotatable, so it must receive nothing", stderrBytes)
	}
	logBytes, err := os.ReadFile(filepath.Join(dir, "spillway.log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(logBytes) != "level=INFO msg=request\n" {
		t.Errorf("the rotated log holds %q; want the line that was written", logBytes)
	}
}

// The other half: when somebody is actually watching a terminal, they still
// see the daemon's output. A fix that silences `spillway server --log-file`
// in a terminal would trade one problem for another.
func TestTerminalStderrStillGetsTheLog(t *testing.T) {
	// A character device stands in for the terminal: os.DevNull is one on
	// every platform this builds for, and no CI runner has a tty.
	dev, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer dev.Close()

	if !duplicateToStderr(dev) {
		t.Error("a character-device stderr gets no copy; a terminal invocation would go silent")
	}

	redirected, err := os.Create(filepath.Join(t.TempDir(), "stderr.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer redirected.Close()
	if duplicateToStderr(redirected) {
		t.Error("a redirected (file) stderr gets a copy; that file is the one nothing can rotate")
	}
}

// The daemon's real logger must take its level from a *slog.LevelVar, not
// from a level fixed at construction.
//
// This is the link that makes demoting the per-request line to debug
// acceptable (#171): internal/proxy proves the line follows a LevelVar, and
// reload.go's syncLogLevel proves a config edit retunes one — but both are
// worthless if runServer hands its handler a plain slog.Level, because then
// the only way to see request traffic is a restart of the daemon whose
// behaviour you are trying to watch.
//
// Parsed, not grepped: the words "LevelVar" appear in a comment two lines
// above the code, so a string match would pass on a file that no longer
// does it.
func TestRunServerHoldsTheLogLevelInALevelVar(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0) // no comments
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	var fn *ast.FuncDecl
	for _, d := range file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "runServer" {
			fn = fd
			break
		}
	}
	if fn == nil {
		t.Fatal("main.go has no func runServer — has it moved or been renamed?")
	}

	// Which identifier, if any, holds a new(slog.LevelVar).
	levelVars := map[string]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		name, ok := as.Lhs[0].(*ast.Ident)
		if !ok {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "new" {
			return true
		}
		if sel, ok := call.Args[0].(*ast.SelectorExpr); ok {
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "slog" && sel.Sel.Name == "LevelVar" {
				levelVars[name.Name] = true
			}
		}
		return true
	})
	if len(levelVars) == 0 {
		t.Fatal("runServer never calls new(slog.LevelVar); log.level would be fixed for the life " +
			"of the process and the per-request tail would need a restart to turn on")
	}

	// And the handler the daemon logs through must be built with it.
	var wired bool
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "slog" || sel.Sel.Name != "HandlerOptions" {
			return true
		}
		for _, e := range lit.Elts {
			kv, ok := e.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if k, ok := kv.Key.(*ast.Ident); !ok || k.Name != "Level" {
				continue
			}
			if id, ok := kv.Value.(*ast.Ident); ok && levelVars[id.Name] {
				wired = true
			}
		}
		return true
	})
	if !wired {
		t.Error("the daemon's slog handler is not built with runServer's *slog.LevelVar; " +
			"log.level would stop applying live (#84, #130) and `log.level: debug` could no " +
			"longer turn the per-request tail on without a restart")
	}
}

// The defaults have to bound the file the service actually opens: the
// daemon reads these straight out of the config and hands them to
// logfile.Open, which refuses anything that bounds nothing.
func TestDefaultConfigOpensABoundedLog(t *testing.T) {
	cfg := config.Defaults()
	lf, err := logfile.Open(filepath.Join(t.TempDir(), "spillway.log"),
		int64(cfg.Log.MaxSizeMB)<<20, cfg.Log.MaxFiles)
	if err != nil {
		t.Fatalf("the shipped defaults cannot open a bounded log: %v", err)
	}
	defer lf.Close()

	if total := int64(cfg.Log.MaxSizeMB) * int64(cfg.Log.MaxFiles); total > 256 {
		t.Errorf("the default bound allows %d MB on disk; #171 is about a log nobody expects "+
			"to be that large", total)
	}
}
