package otel

import (
	"context"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
)

// headerCarrier adapts a nats.Header to the otel TextMapCarrier interface so
// W3C trace context (traceparent/tracestate) and baggage can travel with a
// NATS message. This is what connects a pusher span to its worldsim child
// (and vice versa) across the NATS bridge.
type headerCarrier struct{ h nats.Header }

func (c headerCarrier) Get(key string) string   { return c.h.Get(key) }
func (c headerCarrier) Set(key, val string)     { c.h.Set(key, val) }
func (c headerCarrier) Keys() []string {
	out := make([]string, 0, len(c.h))
	for k := range c.h {
		out = append(out, k)
	}
	return out
}

// Inject writes the active span context from ctx into msg.Header.
// Call before publishing so subscribers can continue the trace.
func Inject(ctx context.Context, msg *nats.Msg) {
	if msg.Header == nil {
		msg.Header = nats.Header{}
	}
	otel.GetTextMapPropagator().Inject(ctx, headerCarrier{msg.Header})
}

// Extract reads span context from msg.Header and returns a new ctx that
// carries it. Call at the start of a subscription handler, then create spans
// from that ctx so they parent to the publisher's span.
func Extract(ctx context.Context, msg *nats.Msg) context.Context {
	if msg == nil || msg.Header == nil {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(ctx, headerCarrier{msg.Header})
}
