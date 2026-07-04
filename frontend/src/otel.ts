// Browser OpenTelemetry setup. Ships spans to a local OTLP/HTTP collector
// (motel by default) so client-side traces are queryable alongside the
// backend Go services.
//
// Env is read at build time via Vite's import.meta.env:
//   VITE_OTEL_ENABLED    "true" to enable (default: false)
//   VITE_OTEL_ENDPOINT   OTLP traces URL (default: http://127.0.0.1:27686/v1/traces)
//   VITE_OTEL_SERVICE    service.name (default: "pixeleruv-frontend")
//
// When disabled, no provider is registered, so trace.getTracer() returns a
// no-op tracer and instrumented call sites stay cheap.
import { context, defaultTextMapGetter, defaultTextMapSetter, trace } from "@opentelemetry/api";
import { ZoneContextManager } from "@opentelemetry/context-zone";
import { W3CTraceContextPropagator, TRACE_PARENT_HEADER } from "@opentelemetry/core";
import { OTLPTraceExporter } from "@opentelemetry/exporter-trace-otlp-http";
import { BatchSpanProcessor, WebTracerProvider } from "@opentelemetry/sdk-trace-web";
import { resourceFromAttributes } from "@opentelemetry/resources";
import { ATTR_SERVICE_NAME } from "@opentelemetry/semantic-conventions";

const enabled = import.meta.env.VITE_OTEL_ENABLED === "true";
const endpoint =
  import.meta.env.VITE_OTEL_ENDPOINT ?? "http://127.0.0.1:27686/v1/traces";
const serviceName = import.meta.env.VITE_OTEL_SERVICE ?? "pixeleruv-frontend";

export function initOtel(): void {
  if (!enabled) return;

  const provider = new WebTracerProvider({
    resource: resourceFromAttributes({ [ATTR_SERVICE_NAME]: serviceName }),
    spanProcessors: [
      new BatchSpanProcessor(
        new OTLPTraceExporter({ url: endpoint }),
        { maxQueueSize: 500, scheduledDelayMillis: 2000 },
      ),
    ],
  });

  provider.register({
    contextManager: new ZoneContextManager(),
  });
}

// tracer is the shared tracer for the app. When OTel is disabled this is a
// no-op tracer, so startSpan/end are cheap no-ops.
export const tracer = trace.getTracer("pixeleruv-web");

const propagator = new W3CTraceContextPropagator();

// traceparentFor returns the W3C traceparent string for the currently active
// span context, or "" when there is none / OTel is disabled. Call this inside
// a span's active context (e.g. via context.with) and put the result in the
// `traceparent` field of an AuthFrame/InputFrame so the backend can parent its
// server-side spans to this browser span.
export function traceparentFor(): string {
  const carrier: Record<string, string> = {};
  propagator.inject(context.active(), carrier, defaultTextMapSetter);
  const v = defaultTextMapGetter.get(carrier, TRACE_PARENT_HEADER);
  return (typeof v === "string" ? v : "") ;
}
