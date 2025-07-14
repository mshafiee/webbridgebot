package config

import (
	"fmt"
	"log"
	"webBridgeBot/internal/reader"

	"github.com/spf13/viper"
)

// DefaultChunkSize is the preferred chunk size for internal processing and caching.
// It must be a multiple of 4096. To avoid potential 'LIMIT_INVALID' errors
// when the Telegram API's upload.getFile method is called, we select a value
// that is 4096 multiplied by a power of 2 (e.g., 64).
// This value is 256 KiB (262144 bytes).
const DefaultChunkSize int64 = 256 * 1024 // 256 KB

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
	LogChannelID   string
}

// InitializeViper sets up Viper to read from environment variables and the .env file.
// This function should be called early in main.
func InitializeViper(logger *log.Logger) {
	viper.AutomaticEnv() // Read environment variables (e.g., from docker-compose)

	// Explicitly set the config file name and type for .env
	viper.SetConfigFile(".env")
	viper.AddConfigPath(".") // Look for .env in the current directory

	if err := viper.ReadInConfig(); err != nil {
		// Log a warning if .env not found. This is normal if config comes from env vars or flags.
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			logger.Printf("Warning: .env config file not found (this is expected if configuration is solely via environment variables or command-line flags).")
		} else {
			logger.Printf("Warning: Error reading config file from .env: %v", err)
		}
	}
	// Note: `viper.BindPFlags` will be called in main.go after flags are defined.
}

// LoadConfig loads configuration from Viper's resolved settings.
// Viper should have already read from files, environment variables, and command-line flags.
func LoadConfig(logger *log.Logger) Configuration {
	var cfg Configuration

	// Direct assignments from Viper's resolved values
	// Use lowercase for Viper keys as they are typically derived from flag names
	// or environment variable names (if using AutomaticEnv).
	cfg.ApiID = viper.GetInt("api_id")
	cfg.ApiHash = viper.GetString("api_hash")
	cfg.BotToken = viper.GetString("bot_token")
	cfg.BaseURL = viper.GetString("base_url")
	cfg.Port = viper.GetString("port")
	cfg.HashLength = viper.GetInt("hash_length")
	cfg.CacheDirectory = viper.GetString("cache_directory")
	cfg.MaxCacheSize = viper.GetInt64("max_cache_size")
	cfg.DebugMode = viper.GetBool("debug_mode")
	cfg.LogChannelID = viper.GetString("log_channel_id")

	// Apply default values if not set by any source (flags, env, file)
	setDefaultValues(&cfg)

	// Validate after all sources (flags, env, defaults) have been applied
	validateMandatoryFields(cfg, logger)

	// Initialize BinaryCache after all config values are final
	initializeBinaryCache(&cfg, logger)

	if cfg.DebugMode {
		logger.Printf("Loaded configuration: %+v", cfg)
	}

	return cfg
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
	// This default for Port is now handled by Cobra's flag definition in main.go
	// but keeping a fallback here is harmless if cfg.Port is somehow still empty
	if cfg.Port == "" {
		cfg.Port = "8080"
	}
}

func initializeBinaryCache(cfg *Configuration, logger *log.Logger) {
	var err error
	cfg.BinaryCache, err = reader.NewBinaryCache(
		cfg.CacheDirectory,
		cfg.MaxCacheSize,
		DefaultChunkSize,
	)
	if err != nil {
		logger.Fatalf("Error initializing BinaryCache: %v", err)
	}
}
