package main

import (
	"cmp"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"slices"
	"strings"
)

// diffKind orders the report: what breaks importers is printed first.
type diffKind int

const (
	removed diffKind = iota
	changed
	added
)

// entry is one difference between the published and the local surface.
type entry struct {
	pkg  string
	name string
	kind diffKind
	was  Symbol // zero for an addition
	now  Symbol // zero for a removal
}

// diff compares two surfaces symbol by symbol. Symbols are keyed by package
// and name only, never by kind: a name that changed kind (a field replaced by
// a method) is one change, not a removal plus an unrelated addition.
func diff(published, local Surface) []entry {
	var entries []entry
	for pkg, syms := range published {
		byName := index(local[pkg])
		for _, was := range syms {
			now, ok := byName[was.Name]
			switch {
			case !ok:
				entries = append(entries, entry{pkg: pkg, name: was.Name, kind: removed, was: was})
			case !compatible(was, now):
				entries = append(entries, entry{pkg: pkg, name: was.Name, kind: changed, was: was, now: now})
			}
		}
	}
	for pkg, syms := range local {
		byName := index(published[pkg])
		for _, now := range syms {
			if _, ok := byName[now.Name]; !ok {
				entries = append(entries, entry{pkg: pkg, name: now.Name, kind: added, now: now})
			}
		}
	}
	slices.SortFunc(entries, func(a, b entry) int {
		return cmp.Or(cmp.Compare(a.kind, b.kind), cmp.Compare(a.pkg, b.pkg), cmp.Compare(a.name, b.name))
	})
	return entries
}

func index(syms []Symbol) map[string]Symbol {
	byName := make(map[string]Symbol, len(syms))
	for _, s := range syms {
		byName[s.Name] = s
	}
	return byName
}

// compatible reports whether two renderings of the same name still describe
// the same API.
func compatible(a, b Symbol) bool {
	return a.Kind == b.Kind && normalize(a) == normalize(b)
}

// normalize reduces a synopsis to the part that actually constrains callers.
// Both sides go through it, so the comparison never turns on which renderer
// produced the text: parameter names are noise, whitespace is noise, and an
// elided composite body is compared as elided on both sides.
func normalize(sym Symbol) string {
	syn := collapse(sym.Synopsis)
	switch sym.Kind {
	case kindConstant:
		// pkg.go.dev publishes "const Name" and nothing more — the value is
		// not part of the surface it records, so we cannot compare one.
		return "const " + sym.Name
	case kindField:
		syn = "var " + syn
	case kindMethod:
		if !strings.HasPrefix(syn, "func") {
			syn = "var " + syn // an interface method, rendered "Name func(...)"
		}
	}
	if norm, ok := normalizeDecl(syn); ok {
		return norm
	}
	return syn
}

// collapse folds a synopsis onto one space-separated line and reduces an
// elided body to its empty form. A type gaining its first exported field goes
// from "struct{}" to "struct{ ... }" without anything about the type itself
// changing; the new field is reported as the addition it is.
func collapse(syn string) string {
	syn = strings.Join(strings.Fields(syn), " ")
	return strings.ReplaceAll(syn, "{ ... }", "{}")
}

// normalizeDecl reparses a collapsed synopsis as a declaration and prints it
// back, so both sides are rendered by the same printer. Function parameter,
// result and receiver names are dropped on the way through: renaming a
// parameter is not an API change, and the published names come from the
// released source rather than the working tree.
//
// It reports false for anything that does not parse — a deeply elided
// synopsis, say — and the caller falls back to comparing collapsed text.
func normalizeDecl(syn string) (string, bool) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "synopsis.go", "package p\n"+syn+"\n", 0)
	if err != nil || len(file.Decls) != 1 {
		return "", false
	}
	if fn, ok := file.Decls[0].(*ast.FuncDecl); ok {
		stripNames(fn.Recv)
		stripNames(fn.Type.Params)
		stripNames(fn.Type.Results)
	}
	return printNode(fset, file.Decls[0]), true
}

// stripNames rewrites a field list down to its types, expanding "a, b string"
// into two anonymous fields so the arity survives.
func stripNames(list *ast.FieldList) {
	if list == nil {
		return
	}
	fields := make([]*ast.Field, 0, len(list.List))
	for _, f := range list.List {
		for range max(len(f.Names), 1) {
			fields = append(fields, &ast.Field{Type: f.Type})
		}
	}
	list.List = fields
}

// section is one class of difference in the report.
type section struct {
	kind    diffKind
	title   string
	detail  string
	comment string
}

var sections = []section{
	{removed, "REMOVED", "published symbols the working tree no longer exports", "every importer of these stops compiling"},
	{changed, "CHANGED", "published symbols whose signature moved", "every importer of these stops compiling"},
	{added, "ADDED", "exported by the working tree, absent from the published surface", "new API, safe for importers"},
}

// printDiff writes the report and returns the process exit code.
func printDiff(w io.Writer, entries []entry, allowBreaking bool) int {
	for _, sec := range sections {
		printSection(w, entries, sec)
	}
	breaking := count(entries, removed) + count(entries, changed)
	printVerdict(w, len(entries), breaking, allowBreaking)
	if breaking > 0 && !allowBreaking {
		return exitBreaking
	}
	return exitOK
}

func printSection(w io.Writer, entries []entry, sec section) {
	matches := 0
	for _, e := range entries {
		if e.kind != sec.kind {
			continue
		}
		if matches == 0 {
			say(w, "%s — %s (%s)\n", sec.title, sec.detail, sec.comment)
		}
		matches++
		say(w, "  %s  %s  %s\n", e.pkg, symbolKind(e), e.name)
		if e.kind != added {
			say(w, "      published: %s\n", e.was.Synopsis)
		}
		if e.kind != removed {
			say(w, "      local:     %s\n", e.now.Synopsis)
		}
	}
	if matches > 0 {
		say(w, "\n")
	}
}

// symbolKind is the kind to print for an entry, taken from whichever side of
// the diff exists.
func symbolKind(e entry) string {
	if e.kind == added {
		return e.now.Kind
	}
	return e.was.Kind
}

func count(entries []entry, kind diffKind) int {
	n := 0
	for _, e := range entries {
		if e.kind == kind {
			n++
		}
	}
	return n
}

// printVerdict states the semver bump the diff implies. The module is v0.x, so
// a minor bump is allowed to carry a breaking change — which is exactly why
// the break has to be said out loud rather than inferred from the version.
func printVerdict(w io.Writer, total, breaking int, allowBreaking bool) {
	switch {
	case total == 0:
		say(w, "no differences: the working tree exports exactly the published surface\n")
		say(w, "verdict: PATCH\n")
		return
	case breaking == 0:
		say(w, "verdict: MINOR — %d addition(s), nothing removed or changed\n", total)
		return
	}

	say(w, "verdict: MINOR (v0.x convention) — but %d symbol(s) BREAK importers.\n", breaking)
	say(w, "         Under semver proper this is a MAJOR bump; while the module is\n")
	say(w, "         v0.x a minor bump may carry it. It is still a break: everything\n")
	say(w, "         importing those symbols stops compiling.\n")
	if allowBreaking {
		say(w, "\n%s is set: reported as a warning, exiting 0.\n", allowBreakingEnv)
		return
	}
	say(w, "\nIf the break is deliberate, re-run with %s=1 to accept it.\n", allowBreakingEnv)
}
