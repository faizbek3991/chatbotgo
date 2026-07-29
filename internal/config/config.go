// Package config loads settings from environment variables (and an optional
// .env file) with no external dependencies.
package config

import (
	"bufio"
	"os"
	"strings"
)

type Config struct {
	GeminiAPIKey string
	GeminiModel  string
	MongoURI     string
	MongoDBName  string
	Port         string
}

// loadDotEnv reads KEY=VALUE lines from path into the process environment.
// Real environment variables are never overwritten. Missing file is fine —
// it just means you're relying on real env vars (e.g. in production).
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.Trim(strings.TrimSpace(parts[1]), `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, val)
		}
	}
}

// Load reads .env (if present) then builds a Config from the environment,
// applying sane defaults for anything optional.
func Load() *Config {
	loadDotEnv(".env")

	cfg := &Config{
		GeminiAPIKey: os.Getenv("GEMINI_API_KEY"),
		GeminiModel:  os.Getenv("GEMINI_MODEL"),
		MongoURI:     os.Getenv("MONGODB_URI"),
		MongoDBName:  os.Getenv("MONGODB_DB"),
		Port:         os.Getenv("PORT"),
	}
	if cfg.GeminiModel == "" {
		cfg.GeminiModel = "gemini-2.0-flash"
	}
	if cfg.MongoDBName == "" {
		cfg.MongoDBName = "gemini_agent"
	}
	if cfg.Port == "" {
		cfg.Port = "8080"
	}
	return cfg
}
