package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

// Data represents the stored name and message.
type Data struct {
	Name    string `json:"name"`
	Message string `json:"message"`
}

const dataFile = "data.json"

var (
	mu   sync.RWMutex
	data Data
)

func main() {
	// Load existing data from file (if any)
	loadData()

	mux := http.NewServeMux()

	// Health check endpoints (for ArgoCD / Kubernetes probes)
	mux.HandleFunc("GET /api/healthz", handleLiveness)
	mux.HandleFunc("GET /api/readyz", handleReadiness)

	// Data endpoints
	mux.HandleFunc("GET /api/data", handleGetData)
	mux.HandleFunc("PATCH /api/data", handlePatchData)

	// Middleware chain: logging → CORS → router
	handler := loggingMiddleware(corsMiddleware(mux))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("🚀 Server starting on :%s", port)
	log.Printf("   GET  /api/healthz  — Liveness probe         → {status}")
	log.Printf("   GET  /api/readyz   — Readiness probe        → {status}")
	log.Printf("   GET  /api/data     — Get current data       → {name, message}")
	log.Printf("   PATCH /api/data    — Update name/message    ← {name?, message?} → {name, message}")

	if err := http.ListenAndServe(":"+port, handler); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}

// --- Health Checks ---

// handleLiveness returns 200 if the process is alive.
// ArgoCD/K8s uses this to know if the container needs a restart.
func handleLiveness(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "alive"})
}

// handleReadiness returns 200 if the server is ready to accept traffic.
// ArgoCD/K8s uses this to know if the pod should receive requests.
func handleReadiness(w http.ResponseWriter, r *http.Request) {
	// Check if we can read/write the data file as a readiness signal
	mu.RLock()
	defer mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
}

// --- Data Endpoints ---

// handleGetData returns the current name and message.
func handleGetData(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	defer mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

// handlePatchData allows partial updates to name and/or message.
func handlePatchData(w http.ResponseWriter, r *http.Request) {
	var patch Data
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		http.Error(w, fmt.Sprintf(`{"error": "invalid JSON: %v"}`, err), http.StatusBadRequest)
		return
	}

	log.Printf("  ← req body: {name: %q, message: %q}", patch.Name, patch.Message)

	mu.Lock()
	defer mu.Unlock()

	// Only update fields that are provided (non-empty)
	if patch.Name != "" {
		data.Name = patch.Name
	}
	if patch.Message != "" {
		data.Message = patch.Message
	}

	// Persist to file
	if err := saveData(); err != nil {
		http.Error(w, fmt.Sprintf(`{"error": "failed to save: %v"}`, err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)

	log.Printf("  → res body: {name: %q, message: %q}", data.Name, data.Message)
}

// --- Persistence ---

func loadData() {
	mu.Lock()
	defer mu.Unlock()

	file, err := os.ReadFile(dataFile)
	if err != nil {
		// File doesn't exist yet — start with defaults
		data = Data{Name: "World", Message: "Hello from Go!"}
		log.Printf("No existing %s found, starting with defaults", dataFile)
		return
	}

	if err := json.Unmarshal(file, &data); err != nil {
		log.Printf("Warning: could not parse %s, starting with defaults: %v", dataFile, err)
		data = Data{Name: "World", Message: "Hello from Go!"}
	}
}

// saveData writes the current data to the JSON file.
// Caller must hold mu.Lock().
func saveData() error {
	file, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(dataFile, file, 0644)
}

// --- CORS Middleware ---

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, PATCH, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		// Handle preflight
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// --- Logging Middleware ---

// statusRecorder wraps http.ResponseWriter to capture the status code.
type statusRecorder struct {
	http.ResponseWriter
	statusCode int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.statusCode = code
	r.ResponseWriter.WriteHeader(code)
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Wrap writer to capture status code
		rec := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(rec, r)

		duration := time.Since(start)
		contentType := rec.Header().Get("Content-Type")

		log.Printf("%s %s → %d (%s) [%s]",
			r.Method,
			r.URL.Path,
			rec.statusCode,
			contentType,
			duration,
		)
	})
}
