package database

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	"backend-server/internal/config"

	_ "github.com/lib/pq"
)

// Connect establishes a connection to PostgreSQL and initializes the schema.
func Connect(cfg *config.Config) *sql.DB {
	if cfg.DBHost == "" {
		log.Fatal("DB_HOST env var is required")
	}

	dsn := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		cfg.DBHost, cfg.DBPort, cfg.DBUser, cfg.DBPassword, cfg.DBName)

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		log.Fatalf("Failed to open DB connection: %v", err)
	}

	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(25)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err = db.Ping(); err != nil {
		log.Printf("Warning: DB unreachable (will retry in Readiness probe): %v", err)
	} else {
		log.Println("✅ Connected to PostgreSQL!")
	}

	migrate(db)
	return db
}

func migrate(db *sql.DB) {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS messages (
			id SERIAL PRIMARY KEY,
			name TEXT NOT NULL,
			message TEXT NOT NULL
		);
	`)
	if err != nil {
		log.Printf("Warning: failed to create table: %v", err)
		return
	}

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&count); err != nil {
		log.Printf("Warning: failed to count rows: %v", err)
	} else if count == 0 {
		if _, err := db.Exec("INSERT INTO messages (name, message) VALUES ($1, $2)", "World", "Hello from Postgres!"); err != nil {
			log.Printf("Warning: failed to insert initial data: %v", err)
		} else {
			log.Println("Initialized DB with default data.")
		}
	}
}
