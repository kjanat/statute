package statute

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"

	"statute.kjanat.dev/resolved"
)

// initTracing builds an OTLP exporter, registers a global TracerProvider, and
// installs the W3C trace-context propagator. The returned shutdown function
// flushes pending spans and closes the exporter; call it during graceful
// shutdown so traces are not dropped on exit.
func initTracing(cfg resolved.Tracing) (shutdown func(context.Context) error, err error) {
	if !cfg.Enabled {
		return func(context.Context) error { return nil }, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	opts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
	}
	if cfg.Insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	exporter, err := otlptrace.New(ctx, otlptracegrpc.NewClient(opts...))
	if err != nil {
		return nil, fmt.Errorf("otlp exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
			attribute.String("statute.version", "0.1.0"),
		),
		resource.WithFromEnv(),
		resource.WithProcess(),
		resource.WithTelemetrySDK(),
	)
	if err != nil {
		return nil, fmt.Errorf("otel resource: %w", err)
	}

	sampler := sdktrace.AlwaysSample()
	if cfg.SampleRate < 1 {
		if cfg.SampleRate <= 0 {
			sampler = sdktrace.NeverSample()
		} else {
			sampler = sdktrace.TraceIDRatioBased(cfg.SampleRate)
		}
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sampler)),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return tp.Shutdown, nil
}

// tracingMiddleware wraps the handler in otelhttp instrumentation. otelhttp
// extracts the incoming W3C trace context, opens a server span, populates
// HTTP semantic-convention attributes, and ends the span when the response
// is committed. Outgoing calls inherit the span via the request context;
// the reverse-proxy transport injects the propagation headers automatically.
func tracingMiddleware(cfg resolved.Tracing, next http.Handler) http.Handler {
	if !cfg.Enabled {
		return next
	}
	return otelhttp.NewHandler(next, "statute.request",
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + r.URL.Path
		}),
	)
}
