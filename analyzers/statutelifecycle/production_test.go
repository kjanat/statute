package statutelifecycle

import (
	"path/filepath"
	"testing"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/packages"
)

func TestProductionLifecycleAudit(t *testing.T) {
	config := &packages.Config{
		Dir:        filepath.Clean(filepath.Join(testdataDir(t), "..", "..", "..")),
		Mode:       packages.NeedName | packages.NeedCompiledGoFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedTypesSizes | packages.NeedImports,
		BuildFlags: []string{"-tags=e2e"},
	}
	loaded, err := packages.Load(config, "./...")
	if err != nil {
		t.Fatal(err)
	}
	if packages.PrintErrors(loaded) != 0 || len(loaded) == 0 {
		t.Fatal("load production packages for lifecycle audit")
	}
	for _, pkg := range loaded {
		t.Run(pkg.PkgPath, func(t *testing.T) {
			pass := &analysis.Pass{
				Analyzer: Analyzer, Fset: pkg.Fset, Files: pkg.Syntax,
				Pkg: pkg.Types, TypesInfo: pkg.TypesInfo, TypesSizes: pkg.TypesSizes,
				Report: func(diagnostic analysis.Diagnostic) {
					t.Errorf("%s: %s", pkg.Fset.Position(diagnostic.Pos), diagnostic.Message)
				},
			}
			if _, err := Analyzer.Run(pass); err != nil {
				t.Fatal(err)
			}
		})
	}
}
