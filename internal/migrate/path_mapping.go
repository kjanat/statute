package migrate

import (
	"path/filepath"
	"strings"
)

func (r *Result) inventoryMounts(source string, document map[string]any) {
	for _, service := range asMap(document["services"]) {
		volumes, _ := asMap(service)["volumes"].([]any)
		mapping := map[string]string{}
		ownsStatic := false
		for _, volume := range volumes {
			from, to := "", ""
			if text, ok := volume.(string); ok {
				parts := strings.Split(text, ":")
				if len(parts) >= 2 {
					from, to = parts[0], parts[1]
				}
			} else {
				values := asMap(volume)
				if values["type"] != "bind" {
					continue
				}
				from, to = stringValue(values["source"]), stringValue(values["target"])
			}
			if from == "" || to == "" || !literal(from) || !literal(to) || !filepath.IsAbs(to) {
				continue
			}
			if !filepath.IsAbs(from) {
				if !strings.HasPrefix(from, ".") {
					continue
				}
				from = filepath.Join(filepath.Dir(source), from)
			}
			from, err := filepath.Abs(from)
			if err != nil {
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
		if r.pathMappings == nil {
			r.pathMappings = map[string][]string{}
		}
		for target, host := range mapping {
			r.pathMappings[target] = append(r.pathMappings[target], host)
		}
	}
}

func pathInside(directory, path string) bool {
	relative, err := filepath.Rel(directory, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (g *generator) mappedProviderPaths(path string) []string {
	// An explicit mapping wins over inferred Compose mounts.
	prefix := ""
	mapped := ""
	for from, to := range g.r.inputs.PathMappings {
		from = filepath.Clean(from)
		if pathInside(from, path) && len(from) > len(prefix) {
			prefix, mapped = from, to
		}
	}
	if prefix != "" {
		relative, _ := filepath.Rel(prefix, path)
		return []string{filepath.Join(mapped, relative)}
	}
	var out []string
	for target, hosts := range g.r.pathMappings {
		if !pathInside(target, path) {
			continue
		}
		relative, _ := filepath.Rel(target, path)
		for _, host := range hosts {
			out = append(out, filepath.Join(host, relative))
		}
	}
	return out
}
