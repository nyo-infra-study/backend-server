package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
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

	// 1. Connect to Database
	connectDB()

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
	log.Printf("   GET  /api/healthz  — Liveness probe")
	log.Printf("   GET  /api/readyz   — Readiness probe")
	log.Printf("   GET  /api/data     — Get current data")
	log.Printf("   PATCH /api/data    — Update name/message")

	if err := http.ListenAndServe(":"+port, handler); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
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
	// We only care about the latest row, or specifically row ID 1 if single-row
	// For simplicity, let's just grab the first row.
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

	// Update logic: we update the first row we find.
	// In a real app we'd target by ID.
	query := "UPDATE messages SET "
	args := []interface{}{}
	argId := 1

	if patch.Name != nil {
		query += fmt.Sprintf("name = $%d, ", argId)
		args = append(args, *patch.Name)
		argId++
	}
	if patch.Message != nil {
		query += fmt.Sprintf("message = $%d, ", argId)
		args = append(args, *patch.Message)
		argId++
	}

	// Remove trailing comma
	if len(args) > 0 {
		query = query[:len(query)-2]
		query += " WHERE id = (SELECT id FROM messages ORDER BY id LIMIT 1)"

		_, err := db.Exec(query, args...)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error": "db update failed: %v"}`, err), http.StatusInternalServerError)
			return
		}
	}

	// Return updated data
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
