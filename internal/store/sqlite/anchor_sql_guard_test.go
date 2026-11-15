package sqlite

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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
//     Commit method itself (its closures included),
//   - any code other than anchorSQL.commit calls Commit on a transaction
//     field (x.tx.Commit()), the field through which CommandTx callbacks and
//     operations reach the same transaction, or
//   - any string literal is a statement that begins or ends a transaction
//     (BEGIN, COMMIT, END, ROLLBACK): a COMMIT or END sent as SQL ends
//     the transaction without Commit, and after a ROLLBACK sent as SQL every
//     later statement of the "transaction" runs in autocommit mode and is
//     durable at once (HA-OPEN7-FINISH).
//
// SQLite makes nothing durable inside a transaction without a COMMIT, and
// database/sql rolls a transaction back when its context ends, so with
// these facts the exception is exact (HA-OPEN7, D381).
func TestAnchorSQLCommitsOnlyThroughItsBoundary(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string][]byte{}
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name) // #nosec G304 -- the package's own source files
		if err != nil {
			t.Fatal(err)
		}
		sources[name] = src
	}
	boundaries, violations := anchorSQLBoundaryViolations(t, sources)
	for _, v := range violations {
		t.Error(v)
	}
	// command.go, replication.go, initial_join.go, authority_transition.go
	// and anchor_local_deny.go build one each today.
	if boundaries < 5 {
		t.Fatalf("found %d functions that build an anchorSQL for a transaction, want at least 5: the guard no longer sees them", boundaries)
	}
}

// The guard fails for each kind of commit around the boundary.
func TestAnchorSQLBoundaryGuardRefusesPlantedCommits(t *testing.T) {
	const boundary = `package sqlite
func (a *anchorSQL) commit() error { a.attempted = true; return a.tx.Commit() }
`
	for name, planted := range map[string]string{
		"a builder commits itself": `package sqlite
func install(tx *sql.Tx) error { op := anchorSQL{tx: tx}; _ = op; return tx.Commit() }`,
		"a callback commits the field": `package sqlite
func callback(c *CommandTx) error { return c.tx.Commit() }`,
		"COMMIT sent as SQL": `package sqlite
func callback(c *CommandTx) error { _, err := c.tx.Exec("COMMIT"); return err }`,
		"END sent as SQL after a statement": "package sqlite\nfunc callback(c *CommandTx) error { _, err := c.tx.Exec(`DELETE FROM x; end transaction`); return err }",
		"ROLLBACK sent as SQL": `package sqlite
func callback(c *CommandTx) error { _, err := c.tx.Exec(" rollback "); return err }`,
	} {
		_, violations := anchorSQLBoundaryViolations(t, map[string][]byte{"boundary.go": []byte(boundary), "planted.go": []byte(planted)})
		if len(violations) == 0 {
			t.Errorf("%s: the guard found nothing", name)
		}
	}
	if _, violations := anchorSQLBoundaryViolations(t, map[string][]byte{"boundary.go": []byte(boundary),
		"ordinary.go": []byte("package sqlite\nconst q = `SELECT name FROM commits WHERE ended = 1`\nconst m = `begin install`\nconst r = `ROLLBACK TO before`\n")}); len(violations) != 0 {
		t.Errorf("an ordinary statement was taken for transaction control: %v", violations)
	}
}

// anchorTransactionControl matches a string literal that is, or contains as
// one of its statements, an SQL statement that begins or ends the
// transaction itself. SAVEPOINT, RELEASE and ROLLBACK TO stay inside the
// transaction that database/sql began, so they are not matched.
var anchorTransactionControl = regexp.MustCompile(`(?i)(^|;)\s*(BEGIN(\s+(DEFERRED|IMMEDIATE|EXCLUSIVE))?|COMMIT|END|ROLLBACK)(\s+TRANSACTION)?\s*(;|$)`)

func anchorSQLBoundaryViolations(t *testing.T, sources map[string][]byte) (int, []string) {
	t.Helper()
	fset := token.NewFileSet()
	boundaries := 0
	var violations []string
	for name, src := range sources {
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if value, err := strconv.Unquote(lit.Value); err == nil && anchorTransactionControl.MatchString(value) {
					violations = append(violations, fset.Position(lit.Pos()).String()+": a transaction-control statement sent as SQL: "+lit.Value)
				}
			}
			return true
		})
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
						violations = append(violations, fset.Position(n.Pos()).String()+": "+fn.Name.Name+" calls Commit on a transaction field outside anchorSQL.commit")
					}
				}
				return true
			})
			if builds {
				boundaries++
				for _, pos := range commits {
					violations = append(violations, fset.Position(pos).String()+": "+fn.Name.Name+" records its transaction in an anchorSQL but commits it itself")
				}
			}
		}
	}
	return boundaries, violations
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
