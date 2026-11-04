package verify

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Coverage is one [Covers] call found in the source of a test.
type Coverage struct {
	// Package directory, relative to the module root, such as "internal/sim".
	Package string
	// Test name, as `go test -run` matches it, with "*" for a subtest whose name is not a literal.
	Test string
	What string
	IDs  []string
	// Pos of the call, as file:line relative to the module root.
	Pos string
}

// Source of the module's Go files, parsed once.
type Source struct {
	root  string
	fset  *token.FileSet
	files []sourceFile
}

type sourceFile struct {
	// dir of the file relative to the root, and whether it is a test file.
	dir  string
	test bool
	file *ast.File
}

// ParseSource of every Go file under root, except in hidden and testdata directories.
func ParseSource(root string) (*Source, error) {
	s := &Source{root: root, fset: token.NewFileSet()}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(s.fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		s.files = append(s.files, sourceFile{dir: filepath.ToSlash(rel), test: strings.HasSuffix(path, "_test.go"), file: f})
		return nil
	})
	return s, err
}

func (s *Source) pos(p token.Pos) string {
	position := s.fset.Position(p)
	rel, err := filepath.Rel(s.root, position.Filename)
	if err != nil {
		rel = position.Filename
	}
	return fmt.Sprintf("%s:%d", filepath.ToSlash(rel), position.Line)
}

// isCall reports whether call is pkg.name(...).
func isCall(call *ast.CallExpr, pkg, name string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg
}

// stringLit is the value of a string literal expression.
func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	return v, err == nil
}

// subtestName as the testing package rewrites it.
func subtestName(name string) string {
	return strings.ReplaceAll(name, " ", "_")
}

// Coverage declared by every [Covers] call in the test files, with an error for each call
// whose arguments are not string literals, that declares no ID, or that is outside a test.
func (s *Source) Coverage() ([]Coverage, []error) {
	var covs []Coverage
	var errs []error
	for _, f := range s.files {
		if !f.test {
			continue
		}
		inTest := map[*ast.CallExpr]bool{}
		for _, decl := range f.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") || fn.Name.Name == "TestMain" {
				continue
			}
			s.walkTest(f, fn.Body, fn.Name.Name, inTest, &covs, &errs)
		}
		ast.Inspect(f.file, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && isCall(call, "verify", "Covers") && !inTest[call] {
				errs = append(errs, fmt.Errorf("%s: verify.Covers outside a test function", s.pos(call.Pos())))
			}
			return true
		})
	}
	sort.SliceStable(covs, func(i, j int) bool {
		if covs[i].Package != covs[j].Package {
			return covs[i].Package < covs[j].Package
		}
		return covs[i].Test < covs[j].Test
	})
	return covs, errs
}

// walkTest finds the Covers calls in body, which runs as the test or subtest path.
func (s *Source) walkTest(f sourceFile, body ast.Node, path string, inTest map[*ast.CallExpr]bool, covs *[]Coverage, errs *[]error) {
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Run" && len(call.Args) == 2 {
			if lit, ok := call.Args[1].(*ast.FuncLit); ok {
				name, ok := stringLit(call.Args[0])
				if !ok {
					name = "*"
				}
				s.walkTest(f, lit.Body, path+"/"+subtestName(name), inTest, covs, errs)
				return false
			}
		}
		if !isCall(call, "verify", "Covers") {
			return true
		}
		inTest[call] = true
		if len(call.Args) < 3 {
			*errs = append(*errs, fmt.Errorf("%s: verify.Covers declares no ID", s.pos(call.Pos())))
			return true
		}
		cov := Coverage{Package: f.dir, Test: path, Pos: s.pos(call.Pos())}
		what, ok := stringLit(call.Args[1])
		if !ok {
			*errs = append(*errs, fmt.Errorf("%s: what verify.Covers asserts must be a string literal", cov.Pos))
			return true
		}
		cov.What = what
		for _, arg := range call.Args[2:] {
			id, ok := stringLit(arg)
			if !ok {
				*errs = append(*errs, fmt.Errorf("%s: the IDs of verify.Covers must be string literals", cov.Pos))
				return true
			}
			cov.IDs = append(cov.IDs, id)
		}
		*covs = append(*covs, cov)
		return true
	})
}

// PackagesWithoutMain are the directories with test files but no TestMain that calls
// [Main].
func (s *Source) PackagesWithoutMain() []string {
	tests := map[string]bool{}
	mains := map[string]bool{}
	for _, f := range s.files {
		if !f.test {
			continue
		}
		tests[f.dir] = true
		for _, decl := range f.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "TestMain" || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok && isCall(call, "verify", "Main") {
					mains[f.dir] = true
				}
				return true
			})
		}
	}
	var missing []string
	for dir := range tests {
		if !mains[dir] {
			missing = append(missing, dir)
		}
	}
	sort.Strings(missing)
	return missing
}

// Assertions are the messages of the Antithesis SDK assertions in the non-test files, by
// assertion function.
func (s *Source) Assertions() map[string][]string {
	messages := map[string][]string{}
	for _, f := range s.files {
		if f.test {
			continue
		}
		ast.Inspect(f.file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			for _, name := range []string{"Always", "AlwaysOrUnreachable", "Sometimes", "Reachable", "Unreachable"} {
				if !isCall(call, "assert", name) {
					continue
				}
				arg := 1
				if name == "Reachable" || name == "Unreachable" {
					arg = 0
				}
				if len(call.Args) > arg {
					if msg, ok := stringLit(call.Args[arg]); ok {
						messages[name] = append(messages[name], msg)
					}
				}
			}
			return true
		})
	}
	return messages
}

// Hits are the states counted by reach.Hit calls in the non-test files, by the name of the
// constant they pass, such as "LeaseOverlap".
func (s *Source) Hits() map[string]bool {
	hits := map[string]bool{}
	for _, f := range s.files {
		if f.test {
			continue
		}
		ast.Inspect(f.file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !isCall(call, "reach", "Hit") || len(call.Args) != 2 {
				return true
			}
			if sel, ok := call.Args[1].(*ast.SelectorExpr); ok {
				hits[sel.Sel.Name] = true
			}
			return true
		})
	}
	return hits
}

// StringConstants declared in the non-test files of a directory, by name.
func (s *Source) StringConstants(dir string) map[string]string {
	consts := map[string]string{}
	for _, f := range s.files {
		if f.test || f.dir != dir {
			continue
		}
		for _, decl := range f.file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if i < len(vs.Values) {
						if v, ok := stringLit(vs.Values[i]); ok {
							consts[name.Name] = v
						}
					}
				}
			}
		}
	}
	return consts
}
