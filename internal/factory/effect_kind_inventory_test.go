package factory_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestUnsafeReplaysCoverEveryPendingEffectKind keeps the reconciliation
// pause table complete: it must hold one unsafe journal entry for exactly the
// kinds the store declares, so a new kind without a fixture fails the suite.
func TestUnsafeReplaysCoverEveryPendingEffectKind(t *testing.T) {
	var covered []store.PendingEffectKind
	for _, replay := range unsafeReplays() {
		covered = append(covered, replay.kind)
	}
	declared := declaredPendingEffectKinds(t)
	if len(covered) != len(declared) {
		t.Fatalf("unsafe replays = %v, want one per declared kind %v", covered, declared)
	}
	if got, want := kindSet(covered), kindSet(declared); !maps.Equal(got, want) {
		t.Fatalf("unsafe replay kinds = %v, want %v", slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)))
	}
}

// declaredPendingEffectKinds parses the store package and returns the value
// of every declared PendingEffectKind constant.
func declaredPendingEffectKinds(t *testing.T) []store.PendingEffectKind {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "store", "*.go"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("find store sources = %v, %v", paths, err)
	}
	var kinds []store.PendingEffectKind
	files := token.NewFileSet()
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(files, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			spec, ok := node.(*ast.ValueSpec)
			if !ok {
				return true
			}
			if kind, ok := spec.Type.(*ast.Ident); !ok || kind.Name != "PendingEffectKind" {
				return false
			}
			for _, value := range spec.Values {
				literal, ok := value.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					t.Fatalf("%s declares a PendingEffectKind that is not a string literal", path)
				}
				unquoted, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", literal.Value, err)
				}
				kinds = append(kinds, store.PendingEffectKind(unquoted))
			}
			return false
		})
	}
	return kinds
}

// kindSet returns the distinct kinds of a list.
func kindSet(kinds []store.PendingEffectKind) map[store.PendingEffectKind]bool {
	set := make(map[store.PendingEffectKind]bool, len(kinds))
	for _, kind := range kinds {
		set[kind] = true
	}
	return set
}
