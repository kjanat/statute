package migrate

import "path/filepath"

func (g *generator) fileProvider(o *Object) {
	if (o.value("filename") == nil) == (o.value(fieldDirectory) == nil) {
		o.note("$inputs", Manual, "file provider requires exactly one filename or directory")
	}
	for _, key := range []string{"filename", fieldDirectory} {
		path := o.text(key)
		if path == "" || !literal(path) {
			continue
		}
		if g.suppliedProviderPath(path, key == fieldDirectory) {
			o.mark(key, Translated, "file-provider input supplied explicitly; compiled Go replaces runtime file reload")
		} else {
			o.mark(key, Manual, "provider path was not supplied as --dynamic; supply the exact file or directory (container paths require explicit remapping)")
		}
	}
	if o.value("watch") != nil {
		o.mark("watch", OutOfScope, "runtime file watching is omitted; regenerate and recompile configuration changes")
	}
	if o.ready() {
		o.Generated = true
	}
}

func (g *generator) suppliedProviderPath(path string, directory bool) bool {
	if g.suppliedProviderPathExact(path, directory) {
		return true
	}
	count := 0
	for _, mapped := range g.mappedProviderPaths(path) {
		if g.suppliedProviderPathExact(mapped, directory) {
			count++
		}
	}
	return count == 1
}

func (g *generator) suppliedProviderPathExact(path string, directory bool) bool {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	if !directory {
		return g.r.documents[absolute] == providerFile
	}
	// Matching an explicitly supplied directory proves that the audit enumerated
	// its complete supported contents. A coincidental file inside it does not.
	for _, input := range g.r.inputs.Dynamic {
		supplied, err := filepath.Abs(input)
		if err == nil && supplied == absolute {
			return true
		}
	}
	return false
}
