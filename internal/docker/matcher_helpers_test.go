package docker

// m is a Traefik envelope/route element: HostTraefik when host is set.
func m(host, path string) Matcher {
	out := Matcher{Host: host, Path: path, PathKind: statutePathKind(path)}
	if host != "" {
		out.HostKind = HostTraefik
	}
	return out
}

func px(host, path string) Matcher {
	out := m(host, path)
	out.PathKind = PathByte
	return out
}

func native(host, path string) Matcher {
	return CompileNative(host, path)
}

func withMW(mt Matcher, names ...string) Matcher {
	mt.Middlewares = names
	return mt
}
