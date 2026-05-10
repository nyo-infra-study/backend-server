package config

import (
	"log"
	"os"

	"github.com/joho/godotenv"
)

// Config holds all application configuration from environment variables.
type Config struct {
	Port                 string
	DBHost               string
	DBPort               string
	DBUser               string
	DBPassword           string
	DBName               string
	OTelEndpoint         string
	PyroscopeURL         string
	OTelServiceName      string
}

// Load reads configuration from environment variables (with optional .env file).
func Load() *Config {
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, relying on environment variables.")
	}

	return &Config{
		Port:            getEnv("PORT", "9000"),
		DBHost:          os.Getenv("DB_HOST"),
		DBPort:          getEnv("DB_PORT", "5432"),
		DBUser:          getEnv("DB_USER", "postgres"),
		DBPassword:      os.Getenv("DB_PASSWORD"),
		DBName:          getEnv("DB_NAME", "postgres"),
		OTelEndpoint:    os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		PyroscopeURL:    os.Getenv("PYROSCOPE_URL"),
		OTelServiceName: getEnv("OTEL_SERVICE_NAME", "backend-server"),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
