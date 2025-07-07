package config

import (
	"fmt"
	"log"
	"webBridgeBot/internal/reader"

	"github.com/spf13/viper"
)

// Set DefaultChunkSize to 512KB to align with Telegram API's upload.getFile limit.
const DefaultChunkSize int64 = 512 * 1024 // 512 KB

type Configuration struct {
	ApiID          int
	ApiHash        string
	BotToken       string
	BaseURL        string
	Port           string
	HashLength     int
	CacheDirectory string
	MaxCacheSize   int64
	DatabasePath   string
	DebugMode      bool
	BinaryCache    *reader.BinaryCache
}

func LoadConfig(logger *log.Logger) Configuration {
	initializeViper(logger)

	var cfg Configuration
	bindViperToConfig(&cfg)
	setDefaultValues(&cfg)               // Apply defaults before validation for consistency
	validateMandatoryFields(cfg, logger) // Now, validation runs on values including defaults
	initializeBinaryCache(&cfg, logger)

	if cfg.DebugMode {
		logger.Printf("Loaded configuration: %+v", cfg)
	}

	return cfg
}

func initializeViper(logger *log.Logger) {
	// viper.SetConfigFile(".env") // Removed, as docker-compose injects directly and run.sh passes flags
	viper.AutomaticEnv() // This still picks up environment variables

	// Although we are passing env vars via docker-compose, keeping ReadInConfig
	// can be useful for local development outside of Docker or if a .env
	// file is intentionally placed inside the container during build.
	// The warning "Error reading config file" is harmless if env vars are otherwise set.
	if err := viper.ReadInConfig(); err != nil {
		logger.Printf("Warning: Error reading config file (this is expected if .env is only used for docker-compose environment variables): %v", err)
	}
}

func bindViperToConfig(cfg *Configuration) {
	cfg.ApiID = viper.GetInt("API_ID")
	cfg.ApiHash = viper.GetString("API_HASH")
	cfg.BotToken = viper.GetString("BOT_TOKEN")
	cfg.BaseURL = viper.GetString("BASE_URL")
	cfg.Port = viper.GetString("PORT")
	cfg.HashLength = viper.GetInt("HASH_LENGTH")
	cfg.CacheDirectory = viper.GetString("CACHE_DIRECTORY")
	cfg.MaxCacheSize = viper.GetInt64("MAX_CACHE_SIZE")
	cfg.DebugMode = viper.GetBool("DEBUG_MODE")
}

func validateMandatoryFields(cfg Configuration, logger *log.Logger) {
	if cfg.ApiID == 0 {
		logger.Fatal("API_ID is required and not set")
	}
	if cfg.ApiHash == "" {
		logger.Fatal("API_HASH is required and not set")
	}
	if cfg.BotToken == "" {
		logger.Fatal("BOT_TOKEN is required and not set")
	}
	if cfg.BaseURL == "" {
		logger.Fatal("BASE_URL is required and not set")
	}
	// PORT is now set with a default, so it's effectively mandatory via the default.
	// If you want to explicitly *require* it from an env var, add a check here.
}

func setDefaultValues(cfg *Configuration) {
	if cfg.HashLength < 6 {
		cfg.HashLength = 8
	}
	if cfg.CacheDirectory == "" {
		cfg.CacheDirectory = ".cache"
	}
	if cfg.MaxCacheSize == 0 {
		cfg.MaxCacheSize = 10 * 1024 * 1024 * 1024 // 10 GB default
	}
	if cfg.DatabasePath == "" {
		cfg.DatabasePath = fmt.Sprintf("%s/webBridgeBot.db", cfg.CacheDirectory)
	}
	// Add default for Port if not set
	if cfg.Port == "" {
		cfg.Port = "8080" // Default port for the web server
	}
}

func initializeBinaryCache(cfg *Configuration, logger *log.Logger) {
	var err error
	cfg.BinaryCache, err = reader.NewBinaryCache(
		cfg.CacheDirectory,
		cfg.MaxCacheSize,
		DefaultChunkSize, // This now correctly uses 512KB
	)
	if err != nil {
		logger.Fatalf("Error initializing BinaryCache: %v", err)
	}
}
