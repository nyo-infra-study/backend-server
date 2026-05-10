package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"

	"backend-server/internal/config"
	"backend-server/internal/database"
	"backend-server/internal/handler"
	"backend-server/internal/middleware"
	"backend-server/internal/telemetry"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

func main() {
	// Load configuration
	cfg := config.Load()

	// Initialize telemetry (OTel + Pyroscope)
	shutdownTelemetry := telemetry.Init(cfg)

	// Connect to database
	db := database.Connect(cfg)

	// Set up HTTP handlers
	h := handler.New(db)
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/healthz", h.Liveness)
	mux.HandleFunc("GET /api/readyz", h.Readiness)
	mux.HandleFunc("GET /api/data", h.GetData)
	mux.HandleFunc("PATCH /api/data", h.PatchData)

	// Middleware chain: OTel tracing → logging → CORS → router
	wrapped := otelhttp.NewHandler(
		middleware.Logging(middleware.CORS(mux)),
		"backend-server",
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return fmt.Sprintf("%s %s", r.Method, r.URL.Path)
		}),
	)

	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: wrapped,
	}

	// Start server
	go func() {
		log.Printf("🚀 Server starting on :%s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server failed: %v", err)
		}
	}()

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt)
	<-quit

	log.Println("Shutting down server...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}

	if shutdownTelemetry != nil {
		log.Println("Flushing telemetry...")
		if err := shutdownTelemetry(ctx); err != nil {
			log.Printf("Failed to shutdown telemetry: %v", err)
		}
	}

	log.Println("Shutdown complete.")
}
