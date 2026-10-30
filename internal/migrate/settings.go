package migrate

import (
	"fmt"
	"math"
	"strconv"

	statute "statute.kjanat.dev"
)

func (g *generator) setting(o *Object) {
	switch o.Name {
	case "accessLog":
		g.accessLog(o)
	case "ping":
		g.ping(o)
	case "global":
		for _, key := range []string{"checkNewVersion", "sendAnonymousUsage"} {
			if _, ok := sourceBoolValue(o.value(key), false); ok {
				o.mark(key, OutOfScope, "Traefik update checks and usage reporting do not apply to Statute")
			}
		}
	case "core":
		classifyCoreSettings(o)
	case "log":
		o.mark("level", OutOfScope, "Traefik internal diagnostic levels do not configure Statute's logger")
	case "api", "dashboard":
		o.mark("", OutOfScope, "Traefik dashboard is outside Statute's scope")
	}
	classifyEmptyProcessSetting(o)
	if o.ready() {
		o.Generated = true
	}
}

func classifyEmptyProcessSetting(o *Object) {
	if o.data != nil && len(o.Fields) == 1 && o.Fields[0].Path == "" && (o.Name == "accessLog" || o.Name == "ping") {
		o.mark("", Translated, "default process setting")
	}
}

func (g *generator) accessLog(o *Object) {
	destination, code, ok := sourceLogDestination(o)
	if !ok {
		return
	}
	format := o.text("format")
	log := statute.JSONLog(destination)
	constructor := "JSONLog"
	if format == "" || format == "common" {
		log = statute.CommonLog(destination)
		constructor = "CommonLog"
	} else if format != "json" {
		o.mark("format", Unsupported, "unsupported access log format")
		return
	}
	code = "statute." + constructor + "(" + code + ")"
	if value := o.value("bufferingSize"); value != nil {
		n, valid := sourceLogBuffer(value)
		if !valid {
			o.mark("bufferingSize", Manual, "log buffer size must be a nonnegative integer")
			return
		}
		log.Buffer(n)
		code += fmt.Sprintf(".Buffer(%d)", n)
	}
	if o.value("filters.statusCodes") != nil {
		values, ok := stringList(o.value("filters.statusCodes"), false)
		if !ok {
			return
		}
		log.Statuses(values...)
		code += ".Statuses(" + quoted(values) + ")"
	}
	g.r.Config.Observability.AccessLog = log
	g.observability = "statute.Observability{AccessLog:" + code + "}"
	for _, key := range []string{"filePath", "format", "bufferingSize", "filters.statusCodes"} {
		o.mark(key, Translated, "native access log destination, queue and status filter")
	}
}

func (g *generator) ping(o *Object) {
	entry := o.text("entryPoint")
	if entry == "" {
		entry = sourceTraefik
	}
	manual, ok := sourceBoolValue(o.value("manualRouting"), false)
	if !ok {
		return
	}
	status := 503
	if value := o.value("terminatingStatusCode"); value != nil {
		var err error
		status, err = integer(value)
		if err != nil || status < 200 || status > 599 {
			return
		}
	}
	if !manual {
		if g.entrypoints[entry] == nil {
			o.mark("entryPoint", Manual, "ping entrypoint is absent from supplied listeners")
			return
		}
		route := statute.MatchExact("/ping").OnListeners(entry).Priority(math.MaxInt).Handle(statute.Ping(status))
		g.internalRoutes = append(g.internalRoutes, route)
		g.internalCode = append(g.internalCode, "statute.MatchExact(\"/ping\").OnListeners("+strconv.Quote(entry)+").Priority("+strconv.Itoa(math.MaxInt)+").Handle(statute.Ping("+strconv.Itoa(status)+"))")
	}
	for _, key := range []string{"entryPoint", "manualRouting", "terminatingStatusCode"} {
		o.mark(key, Translated, "listener-scoped process ping with shutdown status")
	}
}

func classifyCoreSettings(o *Object) {
	if _, ok := sourceBoolValue(o.value("strictTLSOptions"), false); ok {
		o.mark("strictTLSOptions", Translated, "conflicting TLS router policies follow the source conflict rule")
	}
	if syntax := o.text("defaultRuleSyntax"); syntax == "v2" || syntax == "v3" {
		o.mark("defaultRuleSyntax", Translated, "shared literal rule parser")
	}
}

func sourceLogDestination(o *Object) (statute.LogWriter, string, bool) {
	destination := statute.Stdout
	code := "statute.Stdout"
	if path := o.text("filePath"); path != "" {
		if !literal(path) {
			o.mark("filePath", Manual, "log path contains unresolved interpolation")
			return destination, code, false
		}
		destination = statute.LogFile(path)
		code = fmt.Sprintf("statute.LogFile(%q)", path)
	}
	return destination, code, true
}

func sourceLogBuffer(value any) (int, bool) { n, err := integer(value); return n, err == nil && n >= 0 }
