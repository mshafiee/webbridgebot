package config

import (
	"fmt"
	"os"
	"path/filepath"
	"webBridgeBot/internal/logger"
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
	LogLevel       string // Log level: DEBUG, INFO, WARNING, ERROR
	BinaryCache    *reader.BinaryCache
	LogChannelID   string

	// Connection and retry settings
	RequestTimeout int // Timeout for Telegram API requests in seconds
	MaxRetries     int // Maximum number of retry attempts for failed requests
	RetryBaseDelay int // Base delay for exponential backoff in seconds
	MaxRetryDelay  int // Maximum retry delay in seconds
}

// InitializeViper sets up Viper to read from environment variables and the .env file.
// This function should be called early in main.
func InitializeViper(log *logger.Logger, envFilePath string) {
	viper.AutomaticEnv() // Read environment variables (e.g., from docker-compose)

	// Determine which .env file to use
	envFile := findEnvFile(envFilePath, log)

	if envFile != "" {
		viper.SetConfigFile(envFile)
		if err := viper.ReadInConfig(); err != nil {
			log.Infof("Could not read .env file at %s: %v", envFile, err)
			log.Info("Hint: If you need to use a .env file, copy env.sample to .env and configure it.")
			log.Info("Configuration will be loaded from environment variables and command-line flags.")
		} else {
			log.Infof("Successfully loaded configuration from .env file: %s", envFile)
		}
	} else {
		log.Info(".env config file not found (this is expected if configuration is solely via environment variables or command-line flags).")
		log.Info("Configuration will be loaded from environment variables and command-line flags.")
	}
	// Note: `viper.BindPFlags` will be called in main.go after flags are defined.
}

// findEnvFile searches for .env file in multiple locations:
// 1. Custom path specified by user (if provided)
// 2. Executable's directory
// 3. Current working directory
// Returns the first found path or empty string if not found
func findEnvFile(customPath string, log *logger.Logger) string {
	// If custom path is provided, use it directly
	if customPath != "" {
		if _, err := os.Stat(customPath); err == nil {
			return customPath
		}
		log.Warningf("Custom .env file not found at: %s", customPath)
		return ""
	}

	// Try to find .env in multiple locations
	searchPaths := []string{}

	// 1. Executable's directory
	if execPath, err := os.Executable(); err == nil {
		execDir := filepath.Dir(execPath)
		searchPaths = append(searchPaths, filepath.Join(execDir, ".env"))
	}

	// 2. Current working directory
	if cwd, err := os.Getwd(); err == nil {
		searchPaths = append(searchPaths, filepath.Join(cwd, ".env"))
	}

	// Search for .env file
	for _, path := range searchPaths {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}

	return ""
}

// LoadConfig loads configuration from Viper's resolved settings.
// Viper should have already read from files, environment variables, and command-line flags.
func LoadConfig(log *logger.Logger) Configuration {
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
	cfg.LogLevel = viper.GetString("log_level")
	cfg.LogChannelID = viper.GetString("log_channel_id")

	// Connection and retry settings
	cfg.RequestTimeout = viper.GetInt("request_timeout")
	cfg.MaxRetries = viper.GetInt("max_retries")
	cfg.RetryBaseDelay = viper.GetInt("retry_base_delay")
	cfg.MaxRetryDelay = viper.GetInt("max_retry_delay")

	// Apply default values if not set by any source (flags, env, file)
	setDefaultValues(&cfg)

	// Validate after all sources (flags, env, defaults) have been applied
	validateMandatoryFields(cfg, log)

	// Initialize BinaryCache after all config values are final
	initializeBinaryCache(&cfg, log)

	if cfg.DebugMode {
		log.Debugf("Loaded configuration: %+v", cfg)
	}

	return cfg
}

func validateMandatoryFields(cfg Configuration, log *logger.Logger) {
	if cfg.ApiID == 0 {
		log.Fatal("API_ID is required and not set")
	}
	if cfg.ApiHash == "" {
		log.Fatal("API_HASH is required and not set")
	}
	if cfg.BotToken == "" {
		log.Fatal("BOT_TOKEN is required and not set")
	}
	if cfg.BaseURL == "" {
		log.Fatal("BASE_URL is required and not set")
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
	if cfg.LogLevel == "" {
		if cfg.DebugMode {
			cfg.LogLevel = "DEBUG"
		} else {
			cfg.LogLevel = "INFO"
		}
	}

	// Connection and retry defaults
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = 300 // 5 minutes default timeout
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 10 // 10 retry attempts by default (increased for better resilience)
	}
	if cfg.RetryBaseDelay == 0 {
		cfg.RetryBaseDelay = 1 // 1 second base delay
	}
	if cfg.MaxRetryDelay == 0 {
		cfg.MaxRetryDelay = 60 // 60 seconds max delay
	}
}

func initializeBinaryCache(cfg *Configuration, log *logger.Logger) {
	var err error
	cfg.BinaryCache, err = reader.NewBinaryCache(
		cfg.CacheDirectory,
		cfg.MaxCacheSize,
		DefaultChunkSize,
	)
	if err != nil {
		log.Fatalf("Error initializing BinaryCache: %v", err)
	}
}
