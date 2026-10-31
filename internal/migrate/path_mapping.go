package migrate

import (
	"path/filepath"
	"strings"
)

func (r *Result) inventoryMounts(source string, document map[string]any) {
	services := asMap(document["services"])
	for _, name := range keys(services) {
		service := services[name]
		volumes, _ := asMap(service)["volumes"].([]any)
		mapping := map[string]string{}
		ownsStatic := false
		for _, volume := range volumes {
			from, to, ok := composeBindMount(source, volume)
			if !ok {
				continue
			}
			mapping[filepath.Clean(to)] = from
			for _, input := range r.inputs.Static {
				absolute, err := filepath.Abs(input)
				if err == nil && pathInside(from, absolute) {
					ownsStatic = true
				}
			}
		}
		if !ownsStatic {
			continue
		}
		r.pathMappings = append(r.pathMappings, mapping)
	}
}

func pathInside(directory, path string) bool {
	relative, err := filepath.Rel(directory, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (g *generator) mappedProviderPaths(path string) []string {
	// An explicit mapping wins over inferred Compose mounts.
	if mapped, ok := mappedMountPath(g.r.inputs.PathMappings, path); ok {
		return []string{mapped}
	}
	var out []string
	for _, mapping := range g.r.pathMappings {
		if mapped, ok := mappedMountPath(mapping, path); ok {
			out = append(out, mapped)
		}
	}
	return out
}

func mappedMountPath(mapping map[string]string, path string) (string, bool) {
	prefix := ""
	mapped := ""
	for from, to := range mapping {
		from = filepath.Clean(from)
		if pathInside(from, path) && len(from) > len(prefix) {
			prefix, mapped = from, to
		}
	}
	if prefix != "" {
		relative, _ := filepath.Rel(prefix, path)
		return filepath.Join(mapped, relative), true
	}
	return "", false
}

func composeBindMount(source string, volume any) (string, string, bool) {
	from, to := "", ""
	if text, ok := volume.(string); ok {
		parts := strings.Split(text, ":")
		if len(parts) >= 2 {
			from, to = parts[0], parts[1]
		}
	} else {
		values := asMap(volume)
		if values["type"] != "bind" {
			return "", "", false
		}
		from, to = stringValue(values["source"]), stringValue(values["target"])
	}
	if !validComposeBindPath(from, to) {
		return "", "", false
	}
	if !filepath.IsAbs(from) {
		if !strings.HasPrefix(from, ".") {
			return "", "", false
		}
		from = filepath.Join(filepath.Dir(source), from)
	}
	from, err := filepath.Abs(from)
	if err != nil {
		return "", "", false
	}
	return from, to, true
}

func validComposeBindPath(from, to string) bool {
	return from != "" && to != "" && literal(from) && literal(to) && filepath.IsAbs(to)
}
