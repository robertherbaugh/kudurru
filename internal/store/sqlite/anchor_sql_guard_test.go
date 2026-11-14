package sqlite

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// anchorSQL.finish counts the ErrTxDone of its final Rollback as a
// confirmed rollback when the transaction's context ended and no commit was
// attempted (HA-OPEN6, 88a7bc05). That exception to the anchor lane's rule
// ("neither an error nor ErrTxDone is evidence of rollback") holds only if
// no transaction an anchorSQL records is ever committed except through
// anchorSQL.commit, which sets attempted first: a COMMIT sent around it,
// followed by the context's end, would otherwise be reported as rolled
// back. This guard reads the package's source and fails when
//
//   - a function that builds an anchorSQL for a transaction also calls a
//     Commit method itself (its closures included), or
//   - any code other than anchorSQL.commit calls Commit on a transaction
//     field (x.tx.Commit()), the field through which CommandTx callbacks and
//     operations reach the same transaction.
//
// SQLite makes nothing durable without a COMMIT, and database/sql rolls a
// transaction back when its context ends, so with these two facts the
// exception is exact (HA-OPEN7, D381).
func TestAnchorSQLCommitsOnlyThroughItsBoundary(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	boundaries := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name) // #nosec G304 -- the package's own source files
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			isBoundaryCommit := fn.Name.Name == "commit" && fn.Recv != nil && len(fn.Recv.List) == 1 && receiverType(fn.Recv.List[0].Type) == "anchorSQL"
			builds := false
			var commits []token.Pos
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.CompositeLit:
					if id, ok := n.Type.(*ast.Ident); ok && id.Name == "anchorSQL" {
						for _, elt := range n.Elts {
							if kv, ok := elt.(*ast.KeyValueExpr); ok {
								if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "tx" {
									builds = true
								}
							}
						}
					}
				case *ast.CallExpr:
					sel, ok := n.Fun.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "Commit" {
						return true
					}
					commits = append(commits, n.Pos())
					if field, ok := sel.X.(*ast.SelectorExpr); ok && field.Sel.Name == "tx" && !isBoundaryCommit {
						t.Errorf("%s: %s calls Commit on a transaction field outside anchorSQL.commit", fset.Position(n.Pos()), fn.Name.Name)
					}
				}
				return true
			})
			if builds {
				boundaries++
				for _, pos := range commits {
					t.Errorf("%s: %s records its transaction in an anchorSQL but commits it itself", fset.Position(pos), fn.Name.Name)
				}
			}
		}
	}
	// command.go, replication.go, initial_join.go, authority_transition.go
	// and anchor_local_deny.go build one each today.
	if boundaries < 5 {
		t.Fatalf("found %d functions that build an anchorSQL for a transaction, want at least 5: the guard no longer sees them", boundaries)
	}
}

func receiverType(e ast.Expr) string {
	if star, ok := e.(*ast.StarExpr); ok {
		e = star.X
	}
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}
