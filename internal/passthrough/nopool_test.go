package passthrough

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const goMySQLClient = "github.com/go-mysql-org/go-mysql/client"

// poolUses returns where f uses go-mysql's client pool. Bootstrap connections
// must never go in one: a pool reconnect would re-run the *bootstrap* login
// and silently hand back a connection running as the wrong identity.
func poolUses(fset *token.FileSet, f *ast.File) []string {
	var names []string
	var found []string
	for _, imp := range f.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		if path != goMySQLClient {
			continue
		}
		name := "client"
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if name == "." {
			found = append(found, fset.Position(imp.Pos()).String()+": dot-import of the go-mysql client package")
			continue
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return found
	}
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		x, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		for _, name := range names {
			if x.Name == name && (sel.Sel.Name == "NewPool" || strings.HasPrefix(sel.Sel.Name, "Pool")) {
				found = append(found, fset.Position(sel.Pos()).String()+": "+x.Name+"."+sel.Sel.Name)
			}
		}
		return true
	})
	return found
}

func TestPoolUseIsDetected(t *testing.T) {
	for name, src := range map[string]string{
		"NewPool":  `package x; import "github.com/go-mysql-org/go-mysql/client"; var _ = client.NewPool`,
		"type":     `package x; import "github.com/go-mysql-org/go-mysql/client"; var _ *client.Pool`,
		"alias":    `package x; import c "github.com/go-mysql-org/go-mysql/client"; var _ = c.NewPool`,
		"dot":      `package x; import . "github.com/go-mysql-org/go-mysql/client"`,
		"PoolOpts": `package x; import "github.com/go-mysql-org/go-mysql/client"; var _ = client.PoolOption{}`,
	} {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, name+".go", src, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(poolUses(fset, f)) == 0 {
			t.Errorf("%s: pool use not detected", name)
		}
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "ok.go", `package x; import "github.com/go-mysql-org/go-mysql/client"; var _ *client.Conn`, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := poolUses(fset, f); len(got) != 0 {
		t.Errorf("false positive: %v", got)
	}
}

// TestNoClientPoolAnywhere fails if any non-test Go file in the module uses
// go-mysql's client pool. Every upstream connection in this proxy is pinned to
// one session and closed with it; if pooling is ever wanted, the bootstrap
// rule in docs/design/decisions/0004-pass-through-authentication.md has to be
// answered first, and this test is the place that forces the conversation.
func TestNoClientPoolAnywhere(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found at %s: %v", root, err)
	}

	scanned := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly|parser.SkipObjectResolution)
		if perr != nil {
			return nil // not our concern here; the build will complain
		}
		// ImportsOnly skips bodies; reparse fully only when the client package is imported.
		imports := false
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == goMySQLClient {
				imports = true
			}
		}
		scanned++
		if !imports {
			return nil
		}
		f, perr = parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		for _, use := range poolUses(fset, f) {
			t.Errorf("client pool used outside tests: %s", use)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 10 {
		t.Fatalf("only scanned %d Go files under %s; the module walk is broken", scanned, root)
	}
}
