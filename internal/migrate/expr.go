package migrate

import "strings"

type expr struct {
	text  string
	ref   *reusable
	rate  *rateLimitSpec
	group bool
	open  string
	items []expr
	end   string
}

func textExpr(text string) expr { return expr{text: text} }

func (h *reusable) call() string { return h.name + "()" }

func wrapExpr(open string, items []expr, end string) expr {
	return expr{group: true, open: open, items: items, end: end}
}

type renderer map[string]bool

var inline renderer

func (r renderer) render(e expr) string {
	switch {
	case e.ref != nil:
		return e.ref.call()
	case e.rate != nil:
		if r[e.rate.constructor()] {
			return e.rate.call()
		}
		return e.rate.inline()
	case e.group:
		items := make([]string, len(e.items))
		for i, item := range e.items {
			items[i] = r.render(item)
		}
		return e.open + strings.Join(items, ",") + e.end
	default:
		return e.text
	}
}

func (e expr) visit(f func(expr)) {
	f(e)
	for _, item := range e.items {
		item.visit(f)
	}
}

func (e expr) constructors(found map[string]bool) {
	e.visit(func(e expr) {
		if e.rate != nil {
			found[e.rate.constructor()] = true
		}
	})
}
