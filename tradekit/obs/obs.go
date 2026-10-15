// Package obs wires OpenTelemetry to the homelab LGTM stack: traces, metrics and
// logs over OTLP, with logs always teed to stdout for journald.
package obs

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Setup installs global trace/metric/log providers exporting via OTLP when
// OTEL_EXPORTER_OTLP_ENDPOINT is set; logs always go to stdout for journald.
// serviceName labels the logs. Returns a shutdown func. Mirrors the homelab
// convention (system-designer).
func Setup(ctx context.Context, serviceName string) func(context.Context) error {
	stdout := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{})

	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		slog.SetDefault(slog.New(stdout))
		return func(context.Context) error { return nil }
	}

	traceExp, err1 := otlptracehttp.New(ctx)
	metricExp, err2 := otlpmetrichttp.New(ctx)
	logExp, err3 := otlploghttp.New(ctx)
	if err := errors.Join(err1, err2, err3); err != nil {
		slog.SetDefault(slog.New(stdout))
		slog.Error("otel exporter init failed, telemetry disabled", "err", err)
		return func(context.Context) error { return nil }
	}

	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(traceExp))
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)))
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp)))

	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)

	// Fan out logs to stdout and OTLP.
	slog.SetDefault(slog.New(slogTee{stdout, otelslog.NewHandler(serviceName, otelslog.WithLoggerProvider(lp))}))

	if err := runtime.Start(runtime.WithMeterProvider(mp)); err != nil {
		slog.Warn("runtime metrics disabled", "err", err)
	}

	return func(ctx context.Context) error {
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx), lp.Shutdown(ctx))
	}
}

// slogTee sends every record to both handlers.
type slogTee [2]slog.Handler

func (t slogTee) Enabled(ctx context.Context, l slog.Level) bool {
	return t[0].Enabled(ctx, l) || t[1].Enabled(ctx, l)
}
func (t slogTee) Handle(ctx context.Context, r slog.Record) error {
	return errors.Join(t[0].Handle(ctx, r), t[1].Handle(ctx, r))
}
func (t slogTee) WithAttrs(a []slog.Attr) slog.Handler {
	return slogTee{t[0].WithAttrs(a), t[1].WithAttrs(a)}
}
func (t slogTee) WithGroup(name string) slog.Handler {
	return slogTee{t[0].WithGroup(name), t[1].WithGroup(name)}
}
