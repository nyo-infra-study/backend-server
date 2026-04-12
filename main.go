package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/grafana/pyroscope-go"
	"github.com/joho/godotenv"
	_ "github.com/lib/pq"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
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

	// 1. Initialize OpenTelemetry (Traces & Metrics)
	shutdownOTel := initOpenTelemetry()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if shutdownOTel != nil {
			if err := shutdownOTel(ctx); err != nil {
				log.Printf("Failed to shutdown OTel: %v", err)
			}
		}
	}()

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
	// This ensures incoming requests are automatically wrapped in a distributed span!
	handler := otelhttp.NewHandler(loggingMiddleware(corsMiddleware(mux)), "backend-server")

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

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}
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
	traceExporter, err := otlptracegrpc.New(ctx)
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
	metricExporter, err := otlpmetricgrpc.New(ctx)
	if err != nil {
		log.Printf("⚠️  Failed to create metric exporter: %v", err)
		return nil
	}
	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter, sdkmetric.WithInterval(5*time.Second))),
	)
	otel.SetMeterProvider(meterProvider)

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
	var d Data
	err := db.QueryRow("SELECT name, message FROM messages ORDER BY id LIMIT 1").Scan(&d.Name, &d.Message)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error": "db query failed: %v"}`, err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(d)
}

func handlePatchData(w http.ResponseWriter, r *http.Request) {
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

		_, err := db.Exec(query, args...)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error": "db update failed: %v"}`, err), http.StatusInternalServerError)
			return
		}
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
		log.Printf("%s %s [%s]", r.Method, r.URL.Path, time.Since(start))
	})
}
