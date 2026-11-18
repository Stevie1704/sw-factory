package effect_test

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

// TestEffectContractsCoverEveryJournalKind keeps the failure-injection
// contracts complete. The journal kind inventory must match every kind the
// store declares, and the contract table (which drives both the replay and
// the unsafe-replay test) plus the native-resume test must cover exactly that
// inventory, so a new kind without a contract fails the suite.
func TestEffectContractsCoverEveryJournalKind(t *testing.T) {
	requireSameKinds(t, "journalKinds against the store declarations", journalKinds, declaredPendingEffectKinds(t))
	covered := []store.PendingEffectKind{harnessResumeContractKind}
	for _, contract := range effectContracts() {
		covered = append(covered, contract.kind)
	}
	requireSameKinds(t, "failure-injection contracts against journalKinds", covered, journalKinds)
}

// harnessResumeContractKind is the kind TestHarnessResumeFailureInjection
// covers outside the shared contract table.
const harnessResumeContractKind = store.PendingEffectKindHarnessResume

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

// requireSameKinds requires two kind lists to name the same set of kinds.
func requireSameKinds(t *testing.T, what string, got, want []store.PendingEffectKind) {
	t.Helper()
	gotSet, wantSet := kindSet(got), kindSet(want)
	if !maps.Equal(gotSet, wantSet) {
		t.Fatalf("%s: got %v, want %v", what, slices.Sorted(maps.Keys(gotSet)), slices.Sorted(maps.Keys(wantSet)))
	}
}

// kindSet returns the distinct kinds of a list.
func kindSet(kinds []store.PendingEffectKind) map[store.PendingEffectKind]bool {
	set := make(map[store.PendingEffectKind]bool, len(kinds))
	for _, kind := range kinds {
		set[kind] = true
	}
	return set
}
