package config

import (
	"fmt"
	"log"
	"time"

	"github.com/spf13/viper"
	"webBridgeBot/internal/reader"
)

const (
	DefaultChunkSize int64 = 1024 * 1024 // 1 MB
)

type Configuration struct {
	ApiID          int
	ApiHash        string
	BotToken       string
	BaseURL        string
	Port           string
	HashLength     int
	BinaryCache    *reader.BinaryCache
	CacheDirectory string
	MaxCacheSize   int64
	DatabasePath   string
	Timeout        time.Duration
	DebugMode      bool
}

// initializeViper sets up viper with environment variable overrides
func initializeViper() {
	viper.SetConfigFile(".env")
	viper.AutomaticEnv()

	if err := viper.ReadInConfig(); err != nil {
		log.Printf("Error reading config file: %v", err)
	}
}

// validateMandatoryFields checks for mandatory fields and terminates if any are missing
func validateMandatoryFields(config Configuration) {
	if config.ApiID == 0 {
		log.Fatal("API_ID is required and not set")
	}
	if config.ApiHash == "" {
		log.Fatal("API_HASH is required and not set")
	}
	if config.BotToken == "" {
		log.Fatal("BOT_TOKEN is required and not set")
	}
	if config.BaseURL == "" {
		log.Fatal("BASE_URL is required and not set")
	}
}

// setDefaultValues sets default values for optional configuration fields
func setDefaultValues(config *Configuration) {
	if config.HashLength < 6 {
		config.HashLength = 8
	}
	if config.CacheDirectory == "" {
		config.CacheDirectory = ".cache"
	}
	if config.MaxCacheSize == 0 {
		config.MaxCacheSize = 10 * 1024 * 1024 * 1024 // 10 GB default
	}
	if config.DatabasePath == "" {
		config.DatabasePath = fmt.Sprintf("%s/webBridgeBot.db", config.CacheDirectory)
	}
	if config.Timeout == 0 {
		config.Timeout = 30 * time.Second
	}
}

func LoadConfig() Configuration {
	initializeViper()

	config := Configuration{
		ApiID:          viper.GetInt("API_ID"),
		ApiHash:        viper.GetString("API_HASH"),
		BotToken:       viper.GetString("BOT_TOKEN"),
		BaseURL:        viper.GetString("BASE_URL"),
		Port:           viper.GetString("PORT"),
		HashLength:     viper.GetInt("HASH_LENGTH"),
		CacheDirectory: viper.GetString("CACHE_DIRECTORY"),
		MaxCacheSize:   viper.GetInt64("MAX_CACHE_SIZE"),
		Timeout:        viper.GetDuration("TIMEOUT"),
		DebugMode:      viper.GetBool("DEBUG_MODE"),
	}

	validateMandatoryFields(config)
	setDefaultValues(&config)

	var err error
	config.BinaryCache, err = reader.NewBinaryCache(
		config.CacheDirectory,
		config.MaxCacheSize,
		DefaultChunkSize,
	)
	if err != nil {
		log.Fatalf("Error initializing BinaryCache: %v", err)
	}

	if config.DebugMode {
		log.Printf("Loaded configuration: %+v", config)
	}

	return config
}
