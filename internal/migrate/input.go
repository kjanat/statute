package migrate

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"go.yaml.in/yaml/v3"
)

// Inputs are explicit files or dynamic directories. No provider paths are followed.
type Inputs struct{ Static, Dynamic, Compose []string }

const (
	maxInputBytes = 8 << 20
	maxInputFiles = 256
)

var (
	envReference      = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)`)
	wholeEnvReference = regexp.MustCompile(`^\$([A-Za-z_][A-Za-z0-9_]*)$`)
)

// Audit reads only the supplied sources. It never expands environment variables,
// reads referenced secret files, contacts Docker, or executes generated source.
func Audit(inputs Inputs) (*Result, error) {
	result := &Result{Report: Report{Version: 1, Objects: []*Object{}, Checks: []Check{}, Unmatched: []string{}}, inputs: inputs, documents: map[string]string{}}
	count := 0
	for _, group := range []struct {
		kind  string
		paths []string
	}{{providerStatic, inputs.Static}, {providerFile, inputs.Dynamic}, {providerDocker, inputs.Compose}} {
		paths, err := expandPaths(group.paths, group.kind == providerFile)
		if err != nil {
			return nil, err
		}
		for _, path := range paths {
			count++
			if count > maxInputFiles {
				return nil, errors.New("input file limit exceeded")
			}
			data, err := readDocument(path)
			if err != nil {
				return nil, fmt.Errorf("input %q: %w", path, err)
			}
			inventory(&result.Report, path, group.kind, data)
			absolute, err := filepath.Abs(path)
			if err != nil {
				return nil, errors.New("cannot normalize input path")
			}
			result.documents[absolute] = group.kind
		}
	}
	if count == 0 {
		return nil, errors.New("at least one input file is required")
	}
	if err := generate(result); err != nil {
		return nil, err
	}
	return result, nil
}

func expandPaths(paths []string, dirs bool) ([]string, error) {
	var out []string
	for _, path := range paths {
		st, err := os.Lstat(path)
		if err != nil {
			return nil, errors.New("cannot inspect input")
		}
		if st.Mode().IsRegular() {
			out = append(out, path)
			continue
		}
		if !dirs || !st.IsDir() {
			return nil, errors.New("input must be a regular file or dynamic directory")
		}
		found, err := directoryPaths(path)
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}
	slices.Sort(out)
	return out, nil
}

func directoryPaths(path string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(path, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return errors.New("cannot enumerate dynamic directory")
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("dynamic directory contains a symlink")
		}
		switch strings.ToLower(filepath.Ext(p)) {
		case ".yml", ".yaml", ".toml":
			out = append(out, p)
		}
		if len(out) > maxInputFiles {
			return errors.New("input file limit exceeded")
		}
		return nil
	})
	return out, err
}

func readDocument(path string) (map[string]any, error) {
	// #nosec G304 -- These are explicit operator-selected offline inputs.
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open source")
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxInputBytes+1))
	if err != nil || len(b) > maxInputBytes {
		return nil, errors.New("source unreadable or exceeds 8 MiB")
	}
	var data map[string]any
	if strings.EqualFold(filepath.Ext(path), ".toml") {
		err = toml.Unmarshal(b, &data)
	} else {
		d := yaml.NewDecoder(bytes.NewReader(b))
		err = d.Decode(&data)
		if err == nil {
			var extra any
			if !errors.Is(d.Decode(&extra), io.EOF) {
				err = errors.New("multiple YAML documents")
			}
		}
	}
	// Parser errors may include source lines, keys and secret values.
	if err != nil || data == nil {
		return nil, errors.New("invalid or ambiguous YAML/TOML document (details withheld)")
	}
	return data, nil
}

func inventory(r *Report, source, provider string, data map[string]any) {
	if provider == providerDocker {
		inventoryCompose(r, source, data)
		return
	}
	for _, key := range keys(data) {
		v := data[key]
		if provider == providerStatic {
			inventoryStatic(r, source, key, v)
			continue
		}
		if key != protocolHTTP {
			addObject(r, source, provider, key, key, v)
			continue
		}
		if len(asMap(v)) == 0 {
			addObject(r, source, provider, protocolHTTP, protocolHTTP, v)
		}
		for _, kind := range keys(asMap(v)) {
			inventoryGroup(r, source, provider, kind, asMap(v)[kind])
		}
	}
}

func inventoryStatic(r *Report, source, key string, v any) {
	switch strings.ToLower(key) {
	case kindEntrypoints, "certificatesresolvers", kindProviders:
		inventoryGroup(r, source, providerStatic, strings.ToLower(key), v)
	default:
		addObject(r, source, providerStatic, "setting", key, v)
	}
}

func inventoryGroup(r *Report, source, provider, kind string, v any) {
	for _, name := range keys(asMap(v)) {
		addObject(r, source, provider, kind, name, asMap(v)[name])
	}
	if len(asMap(v)) == 0 {
		addObject(r, source, provider, kind, kind, v)
	}
}

func addObject(r *Report, source, provider, kind, name string, value any) *Object {
	invalidName := !literal(name)
	if invalidName {
		name = "<unresolved-name>"
	}
	o := &Object{Source: source, Provider: provider, Kind: kind, Name: name, data: asMap(value), Fields: []Field{}}
	flatten(o, "", value, 0)
	if invalidName {
		o.note("$name", Manual, "unresolved object name; interpolation is not evaluated")
	}
	r.Objects = append(r.Objects, o)
	return o
}

func flatten(o *Object, path string, v any, depth int) {
	if depth > 64 {
		o.note(path, Unsupported, "nesting limit exceeded")
		return
	}
	switch x := v.(type) {
	case map[string]any:
		if len(x) > 0 {
			flattenMap(o, path, x, depth)
			return
		}
	case []any:
		if len(x) > 0 {
			for i, item := range x {
				flatten(o, fmt.Sprintf("%s[%d]", path, i), item, depth+1)
			}
			return
		}
	}
	f := Field{Path: path, Classification: Manual, Reason: "no verified translation; value withheld"}
	f.References = sourceReferences(o, path, v)
	o.Fields = append(o.Fields, f)
}

func flattenMap(o *Object, path string, m map[string]any, depth int) {
	for _, k := range keys(m) {
		p := fieldPath(path, k)
		if o.Provider == providerDocker && depth == 0 {
			p = k
		}
		flatten(o, p, m[k], depth+1)
	}
}

func fieldPath(parent, key string) string {
	if !literal(key) {
		return parent + "[unresolved-key]"
	}
	if strings.ContainsAny(key, ".[]") {
		return parent + "[" + strconv.Quote(key) + "]"
	}
	if parent != "" {
		return parent + "." + key
	}
	return key
}

func sourceReferences(o *Object, path string, v any) []string {
	s, ok := v.(string)
	if !ok {
		return nil
	}
	var refs []string
	for _, m := range envReference.FindAllStringSubmatch(s, -1) {
		refs = append(refs, "env:"+m[1])
	}
	// Embedded bare dollars can be password-hash components, not environment
	// references. Never disclose them as purported variable names.
	if m := wholeEnvReference.FindStringSubmatch(s); m != nil {
		refs = append(refs, "env:"+m[1])
	}
	if knownFileReference(o, path) && literal(s) {
		refs = append(refs, "file:"+s)
	}
	slices.Sort(refs)
	return slices.Compact(refs)
}

func knownFileReference(o *Object, path string) bool {
	if o.Kind == kindProviders && o.Name == providerFile {
		return path == "filename" || path == fieldDirectory
	}
	if o.Kind == "middlewares" {
		return path == "basicAuth.usersFile" || path == "basicauth.usersfile"
	}
	if o.Kind == "serversTransports" {
		return strings.HasPrefix(path, "rootCAs[") || certificateFilePath(path)
	}
	if o.Kind == kindTLS {
		return certificateFilePath(path) || strings.Contains(path, ".clientAuth.caFiles[")
	}
	return false
}

func certificateFilePath(path string) bool {
	return strings.HasSuffix(path, ".certFile") || strings.HasSuffix(path, ".keyFile")
}

func inventoryCompose(r *Report, source string, data map[string]any) {
	services := asMap(data["services"])
	for _, name := range keys(services) {
		inventoryComposeService(r, source, name, asMap(services[name]))
	}
	for _, key := range []string{"include", "merge"} {
		if value, ok := data[key]; ok {
			addObject(r, source, "compose", key, key, value)
		}
	}
}

func inventoryComposeService(r *Report, source, name string, service map[string]any) {
	labels, invalid := composeLabels(service["labels"])
	objects, duplicate := labelObjects(labels)
	invalid = invalid || duplicate
	retainedLabels := traefikLabels(labels)
	for _, id := range keys(objects) {
		kind, obj, _ := strings.Cut(id, ":")
		if kind == kindContainer {
			obj = name
		}
		o := addObject(r, source+"#"+name, providerDocker, kind, obj, objects[id])
		o.labels = retainedLabels
		if invalid {
			o.note("$labels", Unsupported, "ambiguous or malformed Compose labels")
		}
	}
	if invalid && len(objects) == 0 {
		addObject(r, source+"#"+name, providerDocker, kindContainer, name, map[string]any{"$labels": nil})
	}
	for _, key := range []string{"extends", "deploy", "label_file"} {
		if value, ok := service[key]; ok {
			addObject(r, source+"#"+name, "compose", key, name, value)
		}
	}
}

func composeLabels(value any) (map[string]any, bool) {
	switch l := value.(type) {
	case map[string]any:
		return l, false
	case []any:
		labels := map[string]any{}
		invalid := false
		for _, v := range l {
			s, ok := v.(string)
			k, val, found := strings.Cut(s, "=")
			if !ok || !found {
				invalid = true
				continue
			}
			if _, exists := labels[k]; exists {
				invalid = true
			}
			labels[k] = val
		}
		return labels, invalid
	case nil:
		return nil, false
	default:
		return nil, true
	}
}

func labelObjects(labels map[string]any) (map[string]map[string]any, bool) {
	objects := map[string]map[string]any{}
	invalid := false
	for _, k := range keys(labels) {
		if !strings.HasPrefix(strings.ToLower(k), "traefik.") {
			continue
		}
		parts := strings.Split(k, ".")
		for i := range parts {
			if i != 3 {
				parts[i] = strings.ToLower(parts[i])
			}
		}
		kind, obj, field := kindContainer, "labels", strings.Join(parts[1:], ".")
		if len(parts) >= 5 && parts[1] == protocolHTTP {
			kind, obj, field = parts[2], parts[3], strings.Join(parts[4:], ".")
		}
		id := kind + ":" + obj
		if objects[id] == nil {
			objects[id] = map[string]any{}
		}
		if _, exists := objects[id][field]; exists {
			invalid = true
		}
		objects[id][field] = labels[k]
	}
	return objects, invalid
}

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }
func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
func stringValue(v any) string { s, _ := v.(string); return s }
func scalar(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	switch v.(type) {
	case bool, int, int64, float64:
		return fmt.Sprint(v)
	}
	return ""
}
func integer(v any) (int, error) { return strconv.Atoi(scalar(v)) }

func (o *Object) value(path string) any {
	if o.Provider == providerDocker {
		return o.data[strings.ToLower(path)]
	}
	var v any = o.data
	for p := range strings.SplitSeq(path, ".") {
		v = asMap(v)[p]
	}
	return v
}

func (o *Object) text(path string) string { return stringValue(o.value(path)) }
func literal(s string) bool {
	return !strings.ContainsAny(s, "$\r\n\x00") && !strings.Contains(s, "{{")
}

func traefikLabels(labels map[string]any) map[string]string {
	out := map[string]string{}
	for key, value := range labels {
		if strings.HasPrefix(strings.ToLower(key), "traefik.") {
			out[key] = scalar(value)
		}
	}
	return out
}
