package effect

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// replacedServiceMethodCount is the broad coordinator surface removed by the
// journal extraction.
const replacedServiceMethodCount = 30

// TestExportedSurfaceIsSmallerThanTheReplacedServiceMethods keeps the
// journal's named caller-facing declarations below the thirty Service methods
// this module replaced.
func TestExportedSurfaceIsSmallerThanTheReplacedServiceMethods(t *testing.T) {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate effect package source")
	}
	packageDirectory := filepath.Dir(filename)
	packages, err := parser.ParseDir(token.NewFileSet(), packageDirectory, func(info fs.FileInfo) bool {
		return filepath.Ext(info.Name()) == ".go" && !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse effect package: %v", err)
	}
	parsed, ok := packages["effect"]
	if !ok {
		t.Fatal("parsed effect package is missing")
	}
	exported := 0
	for _, file := range parsed.Files {
		for _, declaration := range file.Decls {
			switch value := declaration.(type) {
			case *ast.GenDecl:
				for _, specification := range value.Specs {
					switch named := specification.(type) {
					case *ast.TypeSpec:
						if named.Name.IsExported() {
							exported++
						}
					case *ast.ValueSpec:
						for _, name := range named.Names {
							if name.IsExported() {
								exported++
							}
						}
					}
				}
			case *ast.FuncDecl:
				if value.Name.IsExported() && (value.Recv == nil || exportedReceiver(value.Recv)) {
					exported++
				}
			}
		}
	}
	if exported >= replacedServiceMethodCount {
		t.Fatalf("exported effect declarations = %d, want fewer than %d replaced Service methods", exported, replacedServiceMethodCount)
	}
}

// exportedReceiver reports whether a method belongs to a caller-visible
// named type.
func exportedReceiver(receiver *ast.FieldList) bool {
	if receiver == nil || len(receiver.List) != 1 {
		return false
	}
	value := receiver.List[0].Type
	if pointer, ok := value.(*ast.StarExpr); ok {
		value = pointer.X
	}
	named, ok := value.(*ast.Ident)
	return ok && named.IsExported()
}
