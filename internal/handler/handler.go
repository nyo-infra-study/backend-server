package handler

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

// Data represents the stored name and message.
type Data struct {
	Name    string `json:"name"`
	Message string `json:"message"`
}

// Handler holds dependencies for HTTP handlers.
type Handler struct {
	DB *sql.DB
}

// New creates a Handler with the given database connection.
func New(db *sql.DB) *Handler {
	return &Handler{DB: db}
}

// Liveness is the Kubernetes liveness probe.
func (h *Handler) Liveness(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status": "alive"}`))
}

// Readiness is the Kubernetes readiness probe.
func (h *Handler) Readiness(w http.ResponseWriter, r *http.Request) {
	if err := h.DB.Ping(); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		log.Printf("Readiness check failed: %v", err)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status": "ready"}`))
}

// GetData returns the current data from the database.
func (h *Handler) GetData(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, span := otel.Tracer("backend-server").Start(ctx, "db.query SELECT messages")
	defer span.End()

	var d Data
	err := h.DB.QueryRowContext(ctx, "SELECT name, message FROM messages ORDER BY id LIMIT 1").Scan(&d.Name, &d.Message)
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

// PatchData updates the name and/or message in the database.
func (h *Handler) PatchData(w http.ResponseWriter, r *http.Request) {
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
		query = query[:len(query)-2]
		query += " WHERE id = (SELECT id FROM messages ORDER BY id LIMIT 1)"

		_, dbSpan := otel.Tracer("backend-server").Start(ctx, "db.exec UPDATE messages")
		_, err := h.DB.ExecContext(ctx, query, args...)
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

	h.GetData(w, r)
}
