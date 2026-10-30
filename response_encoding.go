package statute

import (
	"bufio"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"slices"

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
	p.IncludedTypes = append([]string(nil), p.IncludedTypes...)
	p.ExcludedTypes = append([]string(nil), p.ExcludedTypes...)
	if len(p.Encodings) == 0 {
		p.Encodings = []string{codingGzip, "br", codingZstd}
	}
	if err := validateResponseEncodings(p); err != nil {
		return resolved.Middleware{}, err
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
		if responseEncodingBypass(r, requestType, excluded) {
			next.ServeHTTP(w, r)
			return
		}
		coding := selectResponseEncoding(r.Header, p)
		if coding == codingGzip {
			if _, present := cacheHeaderValues(r.Header, "Accept-Encoding"); !present {
				out := r.Clone(r.Context())
				out.Trailer = r.Trailer
				out.Header.Set("Accept-Encoding", codingGzip)
				r = out
			}
			gzipHandler.ServeHTTP(w, r)
			return
		}
		if coding != "br" && coding != codingZstd {
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

func responseEncodingBypass(r *http.Request, requestType string, excluded []string) bool {
	return r.Method == http.MethodHead || !cacheControlAllows(r.Header, "no-transform") || slices.Contains(excluded, requestType)
}

func selectResponseEncoding(h http.Header, p *resolved.EncodingPolicy) string {
	prefs, valid := parseAcceptEncoding(h)
	if !valid {
		return ""
	}
	if !prefs.present {
		return p.DefaultEncoding
	}
	best, bestQ := "", 0
	preference := p.Encodings
	if p.DefaultEncoding != "" {
		if _, explicit := prefs.quality[p.DefaultEncoding]; !explicit {
			preference = append([]string{p.DefaultEncoding}, preference...)
		}
	}
	for _, name := range preference {
		if q := prefs.weight(name); q > bestQ {
			best, bestQ = name, q
		}
	}
	if prefs.quality["identity"] > bestQ {
		return ""
	}
	return best
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
	droppedTrailers headerNameSet
}

func (w *responseEncodingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *responseEncodingWriter) WriteHeader(status int) {
	if w.committed || w.statusSet {
		return
	}
	if status >= 100 && status < 200 {
		w.ResponseWriter.WriteHeader(status)
		if status == http.StatusSwitchingProtocols {
			w.committed = true
			w.hijacked = true
		}
		return
	}
	w.status = status
	w.statusSet = true
}

func (w *responseEncodingWriter) Write(data []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	if w.committed {
		if w.codec != nil {
			return w.codec.Write(data)
		}
		return w.ResponseWriter.Write(data)
	}
	if w.Header().Get("Content-Type") == "" {
		if len(w.prefix)+len(data) < max(512, w.policy.MinimumBytes) {
			w.prefix = append(w.prefix, data...)
			return len(data), nil
		}
		sample := append(slices.Clone(w.prefix[:min(len(w.prefix), 512)]), data[:min(len(data), 512)]...)
		w.Header().Set("Content-Type", http.DetectContentType(sample[:min(len(sample), 512)]))
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
	_, partial := cacheHeaderValues(w.Header(), "Content-Range")
	if w.status == http.StatusPartialContent || partial || !cacheControlAllows(w.Header(), "no-transform") {
		return false
	}
	if responseEncodingBodyless(w.status) || w.Header().Get("Content-Encoding") != "" {
		return false
	}
	text := w.Header().Get("Content-Type")
	kind, _, err := mime.ParseMediaType(text)
	if err != nil {
		return false
	}
	if len(w.policy.IncludedTypes) > 0 {
		return slices.Contains(w.policy.IncludedTypes, kind)
	}
	return kind != "application/grpc" && !slices.Contains(w.policy.ExcludedTypes, kind)
}

func responseEncodingBodyless(status int) bool {
	return status == http.StatusNoContent || status == http.StatusResetContent || status == http.StatusNotModified
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
		w.droppedTrailers = stripCompressionTrailers(w.Header())
		prepareEncodedMetadata(w.Header())
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
	if !w.committed {
		w.detectContentType()
		_ = w.commit(len(w.prefix) > 0 && w.eligible())
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
		w.detectContentType()
		_ = w.commit(len(w.prefix) >= w.policy.MinimumBytes && w.eligible())
	}
	if w.codec != nil {
		dropped := stripCompressionTrailers(w.Header())
		for name := range w.droppedTrailers {
			dropped[name] = struct{}{}
		}
		dropped.removeFrom(w.Header())
		_ = w.codec.Close()
	}
}

func (w *responseEncodingWriter) detectContentType() {
	if len(w.prefix) > 0 && w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", http.DetectContentType(w.prefix))
	}
}

func validateResponseEncodings(p EncodingPolicy) error {
	for _, name := range p.Encodings {
		if name != codingGzip && name != "br" && name != codingZstd {
			return errors.New("encode_response: unsupported encoding")
		}
	}
	if p.DefaultEncoding != "" && !slices.Contains(p.Encodings, p.DefaultEncoding) {
		return errors.New("encode_response: default encoding must be enabled")
	}
	return nil
}
