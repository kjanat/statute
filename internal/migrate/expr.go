package migrate

import "strings"

type expr struct {
	text  string
	rate  *rateLimitSpec
	group bool
	open  string
	items []expr
	end   string
}

func textExpr(text string) expr { return expr{text: text} }

func wrapExpr(open string, items []expr, end string) expr {
	return expr{group: true, open: open, items: items, end: end}
}

type renderer map[string]bool

var inline renderer

func (r renderer) render(e expr) string {
	switch {
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

func (e expr) constructors(found map[string]bool) {
	if e.rate != nil {
		found[e.rate.constructor()] = true
	}
	for _, item := range e.items {
		item.constructors(found)
	}
}
