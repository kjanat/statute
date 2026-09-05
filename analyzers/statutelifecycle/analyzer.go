// Package statutelifecycle provides Statute-specific lifecycle correctness checks.
package statutelifecycle

import (
	"go/ast"
	"go/types"

	"github.com/golangci/plugin-module-register/register"
	"golang.org/x/tools/go/analysis"
)

const (
	pluginName          = "statutelifecycle"
	methodStart         = "start"
	methodStartAPI      = "Start"
	methodStop          = "stop"
	methodStopRun       = "stopRun"
	methodShutdownLocal = "shutdown"
	methodShutdown      = "Shutdown"
	methodClose         = "Close"
	diagnosticSLC100    = "SLC100"
	diagnosticSLC101    = "SLC101"
	diagnosticSLC102    = "SLC102"
	diagnosticSLC103    = "SLC103"
	diagnosticSLC104    = "SLC104"
	diagnosticSLC105    = "SLC105"
	diagnosticSLC106    = "SLC106"
	diagnosticSLC107    = "SLC107"
)

// Analyzer checks Statute-specific lifecycle ownership invariants.
// SLC100 exempts a rollback-owned early publication: a publication rooted at a value whose rollback is already deferred and provably stops and awaits every server it launches is attempt-bracketed, not a leak.
// The evidence is correlated per owner type (the named struct from which the server field, done channel, or WaitGroup is selected): each owner type whose server the publication launches must have one function body in the rollback's transitive call closure containing both a Shutdown/Close on that owner's server and a completion wait on that same owner; a stop and a wait belonging to different owners, or split across unrelated bodies, arm nothing.
var Analyzer = &analysis.Analyzer{
	Name: pluginName,
	Doc:  "trace Statute lifecycle publication, goroutine ownership, cleanup, and Docker mutation invariants",
	Run:  run,
}

func init() {
	register.Plugin(pluginName, newPlugin)
}

type plugin struct{}

func newPlugin(any) (register.LinterPlugin, error) {
	return plugin{}, nil
}

func (plugin) BuildAnalyzers() ([]*analysis.Analyzer, error) {
	return []*analysis.Analyzer{Analyzer}, nil
}

func (plugin) GetLoadMode() string {
	return register.LoadModeTypesInfo
}

type functionInfo struct {
	decl      *ast.FuncDecl
	fn        *types.Func
	publishes bool
}

type cleanupMethod struct {
	fn   *types.Func
	info *functionInfo
}

type lifecycleOwner struct {
	cleanups []cleanupMethod
	result   int // owning result position in the start signature, -1 for the receiver
}

type lifecycleStart struct {
	start        *functionInfo
	ownerResults map[int]bool
	owners       []lifecycleOwner
}

func run(pass *analysis.Pass) (any, error) {
	parents := parentMap(pass.Files)
	functions := collectFunctions(pass)
	propagatePublishers(pass, functions)
	lifecycle := collectLifecycleStarts(functions)

	for _, info := range functions {
		checkPublishBeforeFailure(pass, info, functions)
		checkLifecycleStartCalls(pass, info, lifecycle, parents)
		checkIgnoredLifecycleCalls(pass, info, functions, parents)
	}
	checkGoroutineOwnership(pass, lifecycle)
	checkDockerMutationInvariants(pass, functions, parents)

	return nil, nil
}

func collectFunctions(pass *analysis.Pass) map[*types.Func]*functionInfo {
	out := make(map[*types.Func]*functionInfo)
	for _, file := range pass.Files {
		for _, decl := range file.Decls {
			fnDecl, ok := decl.(*ast.FuncDecl)
			if !ok || fnDecl.Body == nil {
				continue
			}
			fn, _ := pass.TypesInfo.Defs[fnDecl.Name].(*types.Func)
			if fn == nil {
				continue
			}
			out[fn] = &functionInfo{decl: fnDecl, fn: fn}
		}
	}
	return out
}
