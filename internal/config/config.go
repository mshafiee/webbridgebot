package config

import (
	"fmt"
	"log"
	"webBridgeBot/internal/reader"

	"github.com/spf13/viper"
)

const DefaultChunkSize int64 = 1024 * 1024 // 1 MB

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
	validateMandatoryFields(cfg, logger)
	setDefaultValues(&cfg)
	initializeBinaryCache(&cfg, logger)

	if cfg.DebugMode {
		logger.Printf("Loaded configuration: %+v", cfg)
	}

	return cfg
}

func initializeViper(logger *log.Logger) {
	viper.SetConfigFile(".env")
	viper.AutomaticEnv()

	if err := viper.ReadInConfig(); err != nil {
		logger.Printf("Error reading config file: %v", err)
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
