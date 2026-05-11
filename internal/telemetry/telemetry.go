package telemetry

import (
	"context"
	"log"
	"log/slog"
	"os"
	"time"

	"backend-server/internal/config"

	"github.com/grafana/pyroscope-go"
	otelpyroscope "github.com/grafana/otel-profiling-go"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Init sets up structured logging, then initializes OpenTelemetry and Pyroscope.
// Returns a shutdown function that flushes all telemetry on exit.
func Init(cfg *config.Config) func(context.Context) error {
	// Set up structured JSON logging (replaced by OTel bridge if OTel is enabled)
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	// OTel must be initialized first, then Pyroscope.
	// The otelpyroscope wrapper adds pprof labels with span IDs during span execution.
	// Pyroscope SDK then picks up those labels when collecting samples.
	//
	// NOTE: Span-level profiles only work for spans that use more than 10ms of CPU
	// (the pprof sampling interval). Short spans won't have profile data.
	shutdownOTel := initOpenTelemetry(cfg)
	initPyroscope(cfg)

	return shutdownOTel
}

func initOpenTelemetry(cfg *config.Config) func(context.Context) error {
	if cfg.OTelEndpoint == "" {
		log.Println("ℹ️  OTEL_EXPORTER_OTLP_ENDPOINT not set — traces/metrics disabled")
		return nil
	}

	ctx := context.Background()

	res, err := resource.New(ctx,
		resource.WithAttributes(
			attribute.String("service.name", cfg.OTelServiceName),
			attribute.String("service.namespace", getEnvOrDefault("OTEL_SERVICE_NAMESPACE", "dev")),
			attribute.String("service.version", getEnvOrDefault("OTEL_SERVICE_VERSION", "unknown")),
			attribute.String("deployment.environment", getEnvOrDefault("OTEL_DEPLOYMENT_ENVIRONMENT", "dev")),
		),
	)
	if err != nil {
		log.Printf("⚠️  Failed to create OTel resource: %v", err)
		return nil
	}

	// Traces
	traceExporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithRetry(otlptracegrpc.RetryConfig{
			Enabled:         true,
			InitialInterval: 1 * time.Second,
			MaxInterval:     5 * time.Second,
			MaxElapsedTime:  30 * time.Second,
		}),
	)
	if err != nil {
		log.Printf("⚠️  Failed to create trace exporter: %v", err)
		return nil
	}

	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(res),
		sdktrace.WithSpanProcessor(sdktrace.NewBatchSpanProcessor(traceExporter)),
	)

	// Wrap with Pyroscope for span→profile linking
	otel.SetTracerProvider(otelpyroscope.NewTracerProvider(tracerProvider))
	otel.SetTextMapPropagator(propagation.TraceContext{})

	// Metrics
	metricExporter, err := otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithRetry(otlpmetricgrpc.RetryConfig{
			Enabled:         true,
			InitialInterval: 1 * time.Second,
			MaxInterval:     5 * time.Second,
			MaxElapsedTime:  30 * time.Second,
		}),
	)
	if err != nil {
		log.Printf("⚠️  Failed to create metric exporter: %v", err)
		return nil
	}

	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter, sdkmetric.WithInterval(5*time.Second))),
	)
	otel.SetMeterProvider(meterProvider)

	// Go runtime metrics
	if err := runtime.Start(runtime.WithMeterProvider(meterProvider)); err != nil {
		log.Printf("⚠️  Failed to start Go runtime metrics: %v", err)
	}

	// Logs
	logExporter, err := otlploggrpc.New(ctx)
	if err != nil {
		log.Printf("⚠️  Failed to create log exporter: %v", err)
	} else {
		logProvider := sdklog.NewLoggerProvider(
			sdklog.WithResource(res),
			sdklog.WithProcessor(sdklog.NewBatchProcessor(logExporter)),
		)
		slog.SetDefault(otelslog.NewLogger(cfg.OTelServiceName,
			otelslog.WithLoggerProvider(logProvider),
		))
		log.Printf("📝 OpenTelemetry logs started (slog → OTLP)")
	}

	log.Printf("📊 OpenTelemetry metrics/traces started → %s", cfg.OTelEndpoint)

	return func(ctx context.Context) error {
		if err := tracerProvider.Shutdown(ctx); err != nil {
			return err
		}
		return meterProvider.Shutdown(ctx)
	}
}

func getEnvOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func initPyroscope(cfg *config.Config) {
	if cfg.PyroscopeURL == "" {
		log.Println("ℹ️  PYROSCOPE_URL not set — profiling disabled")
		return
	}

	_, err := pyroscope.Start(pyroscope.Config{
		ApplicationName: cfg.OTelServiceName,
		ServerAddress:   cfg.PyroscopeURL,
		ProfileTypes: []pyroscope.ProfileType{
			pyroscope.ProfileCPU,
			pyroscope.ProfileAllocObjects,
			pyroscope.ProfileAllocSpace,
			pyroscope.ProfileInuseObjects,
			pyroscope.ProfileInuseSpace,
			pyroscope.ProfileGoroutines,
		},
	})
	if err != nil {
		log.Printf("⚠️  Pyroscope failed to start: %v", err)
		return
	}
	log.Printf("🔥 Pyroscope profiler started → %s", cfg.PyroscopeURL)
}
