package config

import (
	"fmt"
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
func InitializeViper(log *logger.Logger) {
	viper.AutomaticEnv() // Read environment variables (e.g., from docker-compose)

	// Explicitly set the config file name and type for .env
	viper.SetConfigFile(".env")
	viper.AddConfigPath(".") // Look for .env in the current directory

	if err := viper.ReadInConfig(); err != nil {
		// Log a warning if .env not found. This is normal if config comes from env vars or flags.
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			log.Info(".env config file not found (this is expected if configuration is solely via environment variables or command-line flags).")
		} else {
			// Handle other errors like file not existing or permission issues
			log.Infof("Could not read .env file: %v", err)
			log.Info("Hint: If you need to use a .env file, copy env.sample to .env and configure it.")
		}
		log.Info("Configuration will be loaded from environment variables and command-line flags.")
	} else {
		log.Info("Successfully loaded configuration from .env file")
	}
	// Note: `viper.BindPFlags` will be called in main.go after flags are defined.
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
		cfg.MaxRetries = 5 // 5 retry attempts by default
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
