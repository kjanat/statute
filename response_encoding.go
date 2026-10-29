package statute

import (
	"bufio"
	"cmp"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/gzhttp"
	"github.com/klauspost/compress/zstd"

	"statute.kjanat.dev/resolved"
)

// EncodingPolicy configures response encoding, media-type filters and the minimum
// body size. Encodings are ordered from highest to lowest preference at equal q.
type EncodingPolicy struct {
	Encodings                    []string
	DefaultEncoding              string
	MinimumBytes                 int
	IncludedTypes, ExcludedTypes []string
}

type encodeResponseMW struct{ policy EncodingPolicy }

func (*encodeResponseMW) statuteMiddleware() {}

// EncodeResponse encodes eligible responses and passes through requests that do
// not select an available encoding. HEAD requests pass through unchanged.
func EncodeResponse(policy EncodingPolicy) *encodeResponseMW {
	policy.Encodings = slices.Clone(policy.Encodings)
	policy.IncludedTypes = slices.Clone(policy.IncludedTypes)
	policy.ExcludedTypes = slices.Clone(policy.ExcludedTypes)
	return &encodeResponseMW{policy: policy}
}

func (m *encodeResponseMW) resolve() (resolved.Middleware, error) {
	p := m.policy
	if len(p.Encodings) == 0 {
		p.Encodings = []string{"gzip", "br", "zstd"}
	}
	for _, name := range p.Encodings {
		if name != "gzip" && name != "br" && name != "zstd" {
			return resolved.Middleware{}, errors.New("encode_response: unsupported encoding")
		}
	}
	if p.DefaultEncoding != "" && !slices.Contains(p.Encodings, p.DefaultEncoding) {
		return resolved.Middleware{}, errors.New("encode_response: default encoding must be enabled")
	}
	if p.MinimumBytes < 0 {
		return resolved.Middleware{}, errors.New("encode_response: minimum bytes must be nonnegative")
	}
	if p.MinimumBytes == 0 {
		p.MinimumBytes = 1024
	}
	if len(p.IncludedTypes) > 0 && len(p.ExcludedTypes) > 0 {
		return resolved.Middleware{}, errors.New("encode_response: include and exclude lists are mutually exclusive")
	}
	for _, list := range []*[]string{&p.IncludedTypes, &p.ExcludedTypes} {
		for i, text := range *list {
			kind, _, err := mime.ParseMediaType(text)
			if err != nil {
				return resolved.Middleware{}, errors.New("encode_response: invalid media type")
			}
			(*list)[i] = kind
		}
	}
	return resolved.Middleware{Type: resolved.MWEncodeResponse, Encoding: &resolved.EncodingPolicy{Encodings: p.Encodings, DefaultEncoding: p.DefaultEncoding, MinimumBytes: p.MinimumBytes, IncludedTypes: p.IncludedTypes, ExcludedTypes: p.ExcludedTypes}}, nil
}

func encodeResponseHandler(m resolved.Middleware, next http.Handler) http.Handler {
	if m.Encoding == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	}
	p := m.Encoding
	excluded := append([]string{"application/grpc"}, p.ExcludedTypes...)
	var gzipWrapper func(http.Handler) http.HandlerFunc
	var err error
	if len(p.IncludedTypes) > 0 {
		gzipWrapper, err = gzhttp.NewWrapper(gzhttp.EnableZstd(false), gzhttp.MinSize(p.MinimumBytes), gzhttp.ContentTypes(p.IncludedTypes))
	} else {
		gzipWrapper, err = gzhttp.NewWrapper(gzhttp.EnableZstd(false), gzhttp.MinSize(p.MinimumBytes), gzhttp.ExceptContentTypes(excluded))
	}
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	}
	gzipHandler := gzipWrapper(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if r.Method == http.MethodHead || slices.Contains(excluded, requestType) {
			next.ServeHTTP(w, r)
			return
		}
		coding := selectResponseEncoding(r.Header, p)
		if coding == "gzip" {
			gzipHandler.ServeHTTP(w, r)
			return
		}
		if coding != "br" && coding != "zstd" {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Add("Vary", "Accept-Encoding")
		writer := &responseEncodingWriter{ResponseWriter: w, policy: p, coding: coding, status: http.StatusOK}
		returned := false
		defer func() { writer.finish(returned) }()
		next.ServeHTTP(writer, r)
		returned = true
	})
}

func selectResponseEncoding(h http.Header, p *resolved.EncodingPolicy) string {
	values, present := h["Accept-Encoding"]
	if !present {
		return p.DefaultEncoding
	}
	priority := map[string]int{"*": -1, "identity": len(p.Encodings)}
	for i, name := range p.Encodings {
		priority[name] = i
	}
	type preference struct {
		name string
		q    float64
	}
	var choices []preference
	for _, line := range values {
		for item := range strings.SplitSeq(strings.ReplaceAll(line, " ", ""), ",") {
			name, parameter, _ := strings.Cut(item, ";")
			if _, ok := priority[name]; !ok {
				continue
			}
			q := 1.0
			if raw, ok := strings.CutPrefix(parameter, "q="); ok {
				q, _ = strconv.ParseFloat(raw, 64)
				if q == 0 {
					continue
				}
			}
			choices = append(choices, preference{name, q})
		}
	}
	if len(choices) == 0 {
		return ""
	}
	slices.SortFunc(choices, func(a, b preference) int {
		if a.q == b.q {
			return cmp.Compare(priority[a.name], priority[b.name])
		}
		return cmp.Compare(b.q, a.q)
	})
	if choices[0].name == "*" {
		if p.DefaultEncoding != "" {
			return p.DefaultEncoding
		}
		return p.Encodings[0]
	}
	return choices[0].name
}

type responseEncodingWriter struct {
	http.ResponseWriter
	policy                         *resolved.EncodingPolicy
	coding                         string
	status                         int
	statusSet, committed, hijacked bool
	prefix                         []byte
	codec                          interface {
		io.Writer
		Flush() error
		Close() error
	}
}

func (w *responseEncodingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *responseEncodingWriter) WriteHeader(status int) {
	if status >= 100 && status < 200 {
		w.ResponseWriter.WriteHeader(status)
		if status == http.StatusSwitchingProtocols {
			w.committed = true
			w.hijacked = true
		}
		return
	}
	if !w.statusSet {
		w.status = status
		w.statusSet = true
	}
}

func (w *responseEncodingWriter) Write(data []byte) (int, error) {
	if w.committed {
		if w.codec != nil {
			return w.codec.Write(data)
		}
		return w.ResponseWriter.Write(data)
	}
	if !w.eligible() {
		if err := w.commit(false); err != nil {
			return 0, err
		}
		return w.ResponseWriter.Write(data)
	}
	if len(w.prefix)+len(data) < w.policy.MinimumBytes {
		w.prefix = append(w.prefix, data...)
		return len(data), nil
	}
	if err := w.commit(true); err != nil {
		return 0, err
	}
	if w.codec != nil {
		return w.codec.Write(data)
	}
	return w.ResponseWriter.Write(data)
}

func (w *responseEncodingWriter) eligible() bool {
	if w.status == http.StatusNoContent || w.status == http.StatusResetContent || w.status == http.StatusNotModified || w.Header().Get("Content-Encoding") != "" {
		return false
	}
	text := w.Header().Get("Content-Type")
	if text == "" {
		return true
	}
	kind, _, err := mime.ParseMediaType(text)
	if err != nil {
		return false
	}
	if len(w.policy.IncludedTypes) > 0 {
		return slices.Contains(w.policy.IncludedTypes, kind)
	}
	return kind != "application/grpc" && !slices.Contains(w.policy.ExcludedTypes, kind)
}

func (w *responseEncodingWriter) commit(encode bool) error {
	if w.committed {
		return nil
	}
	if encode {
		if w.coding == "br" {
			w.codec = brotli.NewWriter(w.ResponseWriter)
		} else {
			codec, err := zstd.NewWriter(w.ResponseWriter)
			if err != nil {
				w.status = http.StatusInternalServerError
				w.prefix = nil
				encode = false
			} else {
				w.codec = codec
			}
		}
	}
	if encode {
		w.Header().Del("Content-Length")
		w.Header().Set("Content-Encoding", w.coding)
	}
	w.committed = true
	w.ResponseWriter.WriteHeader(w.status)
	var target io.Writer = w.ResponseWriter
	if w.codec != nil {
		target = w.codec
	}
	if len(w.prefix) > 0 {
		_, err := target.Write(w.prefix)
		w.prefix = nil
		return err
	}
	return nil
}

func (w *responseEncodingWriter) Flush() {
	if len(w.prefix) == 0 && !w.committed {
		return
	}
	if !w.committed {
		_ = w.commit(w.eligible())
	}
	if w.codec != nil {
		_ = w.codec.Flush()
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *responseEncodingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.hijacked = true
	}
	return conn, rw, err
}

func (w *responseEncodingWriter) finish(returned bool) {
	if !returned || w.hijacked {
		return
	}
	if !w.committed {
		_ = w.commit(false)
	}
	if w.codec != nil {
		_ = w.codec.Close()
	}
}
