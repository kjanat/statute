package migrate

func appendMiddlewarePlan(dst *middleware, child middleware) {
	if dst.redirect != nil && (len(child.values) > 0 || child.redirect != nil) {
		dst.continuation = true
	}
	if child.redirect != nil {
		dst.redirect = child.redirect
	}
	dst.continuation = dst.continuation || child.continuation
	dst.incomplete = dst.incomplete || child.incomplete
	dst.values = append(dst.values, child.values...)
	dst.code = append(dst.code, child.code...)
}

func (g *generator) sourceMiddlewarePlan(refs []string, provider string) (middleware, bool) {
	var plan middleware
	for _, ref := range refs {
		id := qualified(ref, provider)
		child, found := g.chains[id]
		if action, ok := g.routeChains[id]; ok {
			child, found = action, true
		}
		if !found || len(plan.values)+len(child.values) > 256 {
			plan.incomplete = true
			continue
		}
		appendMiddlewarePlan(&plan, child)
	}
	return plan, !plan.incomplete
}

func (g *generator) prepareRedirectDefinition(o *Object) bool {
	family := ""
	for _, name := range []string{redirectFamilyScheme, redirectFamilyRegex} {
		if o.value(name) != nil || hasFieldPrefix(o, name+".") {
			family = name
		}
	}
	if family == "" {
		return false
	}
	target, _, _, reason := redirectCandidate(o)
	if target == "" {
		o.mark(family, Manual, reason)
		return true
	}
	classifyRedirectFields(o, family)
	if reason != "" {
		o.note("$redirect-match", Manual, reason)
	}
	return true
}

func classifyRedirectFields(o *Object, family string) {
	fields := []string{"regex", "replacement", fieldPermanent, "statusCode"}
	if family == redirectFamilyScheme {
		fields = []string{"scheme", "port", fieldPermanent, "forcePermanentRedirect"}
	}
	for _, field := range fields {
		o.mark(family+"."+field, Translated, "native redirect action with capture template and explicit method statuses")
	}
}
