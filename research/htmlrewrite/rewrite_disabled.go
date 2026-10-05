//go:build !statute_htmlrewrite

package htmlrewrite

import (
	"context"
	"errors"
	"io"
)

var errRewriteUnavailable = errors.New("HTML rewriting is unavailable: rebuild with -tags statute_htmlrewrite")

type engine struct{}
type stream struct{}

func newEngine(context.Context) (*engine, error) { return nil, errRewriteUnavailable }
func (*engine) close() error                     { return nil }
func (*engine) newStream(context.Context, io.Writer, int) (*stream, error) {
	return nil, errRewriteUnavailable
}
func (*stream) write([]byte) error { return errRewriteUnavailable }
func (*stream) finish() error      { return errRewriteUnavailable }
func (*stream) close() error       { return nil }
