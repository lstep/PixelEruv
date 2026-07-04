// Package otel wires OpenTelemetry traces and logs to a local OTLP/HTTP
// collector (motel by default). It is opt-in: set OTEL_ENABLED=true to arm
// the exporters. When disabled, Init returns a no-op logger and shutdown.
//
// Env vars:
//
//	OTEL_ENABLED              "true" to enable (default: false)
//	OTEL_EXPORTER_OTLP_ENDPOINT  OTLP base URL (default: http://127.0.0.1:27686)
//	OTEL_SERVICE_NAME         override service.name (default: caller-provided)
//	OTEL_TRACES_SAMPLE_RATIO  0.0–1.0 TraceIDRatio ratio (default: 1.0)
package otel

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.30.0"
)

// Init configures global TracerProvider, LoggerProvider, and the
// W3C TraceContext propagator. It returns a slog.Logger whose records are
// emitted as OTel logs (correlated to the active span) and a shutdown func
// that flushes pending telemetry. When OTEL_ENABLED != "true", telemetry is
// disabled and a no-op logger is returned.
func Init(ctx context.Context, serviceName string) (*slog.Logger, func(context.Context) error, error) {
	// Always install the W3C propagator so context injection/extraction works
	// even when exporters are off (no-op spans still carry trace context).
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	if !enabled() {
		// No-op logger; spans are also no-op via the default TracerProvider.
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})),
			func(context.Context) error { return nil }, nil
	}

	endpoint, err := parseEndpoint(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	if err != nil {
		return nil, nil, err
	}

	name := serviceName
	if v := os.Getenv("OTEL_SERVICE_NAME"); v != "" {
		name = v
	}

	res, err := sdkresource.New(ctx,
		sdkresource.WithAttributes(semconv.ServiceName(name)),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("otel resource: %w", err)
	}

	// --- Traces ---
	traceExp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(endpoint),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("otel trace exporter: %w", err)
	}
	ratio := sampleRatio()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.TraceIDRatioBased(ratio)),
	)
	otel.SetTracerProvider(tp)

	// --- Logs ---
	logExp, err := otlploghttp.New(ctx,
		otlploghttp.WithEndpoint(endpoint),
		otlploghttp.WithInsecure(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("otel log exporter: %w", err)
	}
	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp)),
		sdklog.WithResource(res),
	)
	global.SetLoggerProvider(lp)

	logger := otelslog.NewLogger(serviceName)

	shutdown := func(ctx context.Context) error {
		var errs []error
		if err := tp.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
		if err := lp.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
		if len(errs) > 0 {
			return fmt.Errorf("otel shutdown: %v", errs)
		}
		return nil
	}
	return logger, shutdown, nil
}

func enabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_ENABLED")))
	return v == "true" || v == "1" || v == "yes"
}

// parseEndpoint turns OTEL_EXPORTER_OTLP_ENDPOINT (e.g.
// "http://127.0.0.1:27686") into the "host:port" form required by the
// OTLP HTTP exporter options. A bare "host:port" is also accepted.
func parseEndpoint(raw string) (string, error) {
	if raw == "" {
		return "127.0.0.1:27686", nil
	}
	if !strings.Contains(raw, "://") {
		return raw, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("otel endpoint: %w", err)
	}
	return u.Host, nil
}

func sampleRatio() float64 {
	v := os.Getenv("OTEL_TRACES_SAMPLE_RATIO")
	if v == "" {
		return 1.0
	}
	r, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 1.0
	}
	if r < 0 {
		return 0
	}
	if r > 1 {
		return 1
	}
	return r
}

// FlushTimeout is the deadline used by Shutdown when callers don't supply one.
const FlushTimeout = 5 * time.Second
