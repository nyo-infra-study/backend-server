package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/grafana/pyroscope-go"
	"github.com/joho/godotenv"
	_ "github.com/lib/pq"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
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
	"go.opentelemetry.io/otel/trace"
)

// Data represents the stored name and message.
type Data struct {
	Name    string `json:"name"`
	Message string `json:"message"`
}

var db *sql.DB

func main() {
	// 0. Load .env file (for local dev)
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, relying on environment variables.")
	}

	// Set up structured JSON logging for trace correlation
	// Will be replaced with OTel slog bridge after OTel init
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	// 1. Initialize OpenTelemetry (Traces & Metrics & Logs)
	shutdownOTel := initOpenTelemetry()
	// Shutdown is now handled explicitly at the end of main() to ensure final flush

	// 2. Initialize Pyroscope profiler (no-op if PYROSCOPE_URL is not set)
	initPyroscope()

	// 3. Connect to Database
	connectDB()

	mux := http.NewServeMux()

	// Health check endpoints (for ArgoCD / Kubernetes probes)
	// We don't trace health checks to avoid noise
	mux.HandleFunc("GET /api/healthz", handleLiveness)
	mux.HandleFunc("GET /api/readyz", handleReadiness)

	// Data endpoints
	mux.HandleFunc("GET /api/data", handleGetData)
	mux.HandleFunc("PATCH /api/data", handlePatchData)

	// Middleware chain: OTel tracing -> logging → CORS → router
	// Use WithSpanNameFormatter to get the actual route in the span name
	handler := otelhttp.NewHandler(
		loggingMiddleware(corsMiddleware(mux)),
		"backend-server",
		otelhttp.WithSpanNameFormatter(func(operation string, r *http.Request) string {
			return fmt.Sprintf("%s %s", r.Method, r.URL.Path)
		}),
	)

	port := os.Getenv("PORT")
	if port == "" {
		port = "9000"
	}

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: handler,
	}

	go func() {
		log.Printf("🚀 Server starting on :%s", port)
		log.Printf("   GET  /api/healthz  — Liveness probe")
		log.Printf("   GET  /api/readyz   — Readiness probe")
		log.Printf("   GET  /api/data     — Get current data")
		log.Printf("   PATCH /api/data    — Update name/message")

		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server failed: %v", err)
		}
	}()

	// Wait for interrupt signal to gracefully shutdown the server
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt)
	<-quit
	log.Println("Shutting down server...")

	// Create a context for graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 1. Stop accepting new requests
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}
	log.Println("HTTP server stopped.")

	// 2. Flush and shutdown OTel (Captures final spans/metrics from the shutdown itself)
	if shutdownOTel != nil {
		log.Println("Flushing telemetry...")
		if err := shutdownOTel(ctx); err != nil {
			log.Printf("Failed to shutdown OTel: %v", err)
		}
	}
	log.Println("Shutdown complete.")
}

// initOpenTelemetry configures the OTel SDK to export to the collector via gRPC
func initOpenTelemetry() func(context.Context) error {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		log.Println("ℹ️  OTEL_EXPORTER_OTLP_ENDPOINT not set — traces/metrics disabled")
		return nil
	}

	ctx := context.Background()

	// 1. Identify the application
	res, err := resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", "backend-server")),
	)
	if err != nil {
		log.Printf("⚠️  Failed to create OTel resource: %v", err)
		return nil
	}

	// 2. Set up Trace Provider pushing to OTLP
	// Added retry logic for resilience if the collector is temporarily down
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
	bsp := sdktrace.NewBatchSpanProcessor(traceExporter)
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(res),
		sdktrace.WithSpanProcessor(bsp),
	)
	otel.SetTracerProvider(tracerProvider)

	// Propagator ensures traces cross service boundaries via HTTP headers
	otel.SetTextMapPropagator(propagation.TraceContext{})

	// 3. Set up Metric Provider pushing to OTLP
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
	
	// 4. Start Go runtime metrics instrumentation
	if err := runtime.Start(runtime.WithMeterProvider(meterProvider)); err != nil {
		log.Printf("⚠️  Failed to start Go runtime metrics context: %v", err)
	}

	// 5. Set up Log Provider pushing to OTLP
	logExporter, err := otlploggrpc.New(ctx)
	if err != nil {
		log.Printf("⚠️  Failed to create log exporter: %v", err)
	} else {
		logProvider := sdklog.NewLoggerProvider(
			sdklog.WithResource(res),
			sdklog.WithProcessor(sdklog.NewBatchProcessor(logExporter)),
		)
		// Replace default slog with OTel bridge — logs now export via OTLP
		// with automatic trace_id/span_id correlation
		slog.SetDefault(otelslog.NewLogger("backend-server",
			otelslog.WithLoggerProvider(logProvider),
		))
		log.Printf("📝 OpenTelemetry logs started (slog → OTLP)")
	}

	log.Printf("📊 OpenTelemetry metrics/traces started → %s", endpoint)

	// Return a shutdown function for graceful exit
	return func(ctx context.Context) error {
		if err := tracerProvider.Shutdown(ctx); err != nil {
			return err
		}
		return meterProvider.Shutdown(ctx)
	}
}

// initPyroscope starts the Pyroscope continuous profiler.
func initPyroscope() {
	pyroscopeURL := os.Getenv("PYROSCOPE_URL")
	if pyroscopeURL == "" {
		log.Println("ℹ️  PYROSCOPE_URL not set — profiling disabled")
		return
	}

	_, err := pyroscope.Start(pyroscope.Config{
		ApplicationName: "backend-server",
		ServerAddress:   pyroscopeURL,
		Logger:          nil, // use default (no-op)
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
	log.Printf("🔥 Pyroscope profiler started → %s", pyroscopeURL)
}

func connectDB() {
	host := os.Getenv("DB_HOST")
	port := os.Getenv("DB_PORT")
	user := os.Getenv("DB_USER")
	password := os.Getenv("DB_PASSWORD")
	dbname := os.Getenv("DB_NAME")

	if host == "" {
		log.Fatal("DB_HOST env var is required")
	}

	psqlInfo := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		host, port, user, password, dbname)

	var err error
	db, err = sql.Open("postgres", psqlInfo)
	if err != nil {
		log.Fatalf("Failed to open DB connection: %v", err)
	}

	// Performance Optimization: Cap the memory and connection overhead
	db.SetMaxOpenConns(25)                 // Max concurrent connections to the DB
	db.SetMaxIdleConns(25)                 // Max idle connections to keep open
	db.SetConnMaxLifetime(5 * time.Minute) // Recycle connections safely to prevent leaks

	// Verify connection
	if err = db.Ping(); err != nil {
		log.Printf("Warning: DB unreachable (will retry in Readiness probe): %v", err)
	} else {
		log.Println("✅ Connected to PostgreSQL!")
	}

	// Create table if not exists
	createTableQuery := `
	CREATE TABLE IF NOT EXISTS messages (
		id SERIAL PRIMARY KEY,
		name TEXT NOT NULL,
		message TEXT NOT NULL
	);
	`
	_, err = db.Exec(createTableQuery)
	if err != nil {
		log.Printf("Warning: failed to create table: %v", err)
	}

	// Ensure at least one row exists
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&count)
	if err != nil {
		log.Printf("Warning: failed to count rows: %v", err)
	} else if count == 0 {
		_, err = db.Exec("INSERT INTO messages (name, message) VALUES ($1, $2)", "World", "Hello from Postgres!")
		if err != nil {
			log.Printf("Warning: failed to insert initial data: %v", err)
		} else {
			log.Println("Initialized DB with default data.")
		}
	}
}

// --- Health Checks ---

func handleLiveness(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status": "alive"}`))
}

func handleReadiness(w http.ResponseWriter, r *http.Request) {
	if err := db.Ping(); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		log.Printf("Readiness check failed: %v", err)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status": "ready"}`))
}

// --- Data Endpoints ---

func handleGetData(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, span := otel.Tracer("backend-server").Start(ctx, "db.query SELECT messages")
	defer span.End()

	var d Data
	err := db.QueryRowContext(ctx, "SELECT name, message FROM messages ORDER BY id LIMIT 1").Scan(&d.Name, &d.Message)
	if err != nil {
		span.RecordError(err)
		http.Error(w, fmt.Sprintf(`{"error": "db query failed: %v"}`, err), http.StatusInternalServerError)
		return
	}

	span.SetAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation", "SELECT"),
		attribute.String("db.sql.table", "messages"),
	)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(d)
}

func handlePatchData(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var patch struct {
		Name    *string `json:"name"`
		Message *string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		http.Error(w, `{"error": "invalid JSON"}`, http.StatusBadRequest)
		return
	}

	var queryBuilder strings.Builder
	queryBuilder.WriteString("UPDATE messages SET ")
	args := []interface{}{}
	argId := 1

	if patch.Name != nil {
		queryBuilder.WriteString(fmt.Sprintf("name = $%d, ", argId))
		args = append(args, *patch.Name)
		argId++
	}
	if patch.Message != nil {
		queryBuilder.WriteString(fmt.Sprintf("message = $%d, ", argId))
		args = append(args, *patch.Message)
		argId++
	}

	if len(args) > 0 {
		query := queryBuilder.String()
		query = query[:len(query)-2] // Remove trailing comma and space
		query += " WHERE id = (SELECT id FROM messages ORDER BY id LIMIT 1)"

		_, dbSpan := otel.Tracer("backend-server").Start(ctx, "db.exec UPDATE messages")
		_, err := db.ExecContext(ctx, query, args...)
		if err != nil {
			dbSpan.RecordError(err)
			dbSpan.End()
			http.Error(w, fmt.Sprintf(`{"error": "db update failed: %v"}`, err), http.StatusInternalServerError)
			return
		}
		dbSpan.SetAttributes(
			attribute.String("db.system", "postgresql"),
			attribute.String("db.operation", "UPDATE"),
			attribute.String("db.sql.table", "messages"),
			attribute.String("db.statement", query),
		)
		dbSpan.End()
	}

	handleGetData(w, r)
}

// --- Middleware ---

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, PATCH, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		duration := time.Since(start)

		// Extract trace context for log-trace correlation in SigNoz
		spanCtx := trace.SpanFromContext(r.Context()).SpanContext()
		attrs := []slog.Attr{
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.String("duration", duration.String()),
		}
		if spanCtx.HasTraceID() {
			attrs = append(attrs,
				slog.String("trace_id", spanCtx.TraceID().String()),
				slog.String("span_id", spanCtx.SpanID().String()),
			)
		}
		slog.LogAttrs(r.Context(), slog.LevelInfo, "http request", attrs...)
	})
}
