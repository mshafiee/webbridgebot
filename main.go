package main

import (
	"fmt"
	"log"
	"os"
	"webBridgeBot/internal/bot"
	"webBridgeBot/internal/config" // Import config package

	"github.com/spf13/cobra"
	"github.com/spf13/viper" // Import viper for BindPFlags
)

// cfg is declared at the package level to allow Cobra to bind flags directly to its fields.
var cfg config.Configuration

func main() {
	logger := log.New(os.Stdout, "webBridgeBot: ", log.Ldate|log.Ltime|log.Lshortfile)

	// 1. Initialize Viper: Read from environment variables and .env file.
	// This happens *before* Cobra parses flags, so flags will take precedence.
	config.InitializeViper(logger)

	rootCmd := &cobra.Command{
		Use:   "webBridgeBot",
		Short: "WebBridgeBot",
		// PersistentPreRunE is called before the Run function of any command (root or sub).
		// It's the ideal place to bind flags to Viper.
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// 2. Bind Cobra flags to Viper:
			// This tells Viper to consider command-line flags as a configuration source.
			// Flags will typically override values from environment variables or config files.
			return viper.BindPFlags(cmd.Flags())
		},
		Run: func(cmd *cobra.Command, args []string) {
			// 3. Load final configuration:
			// config.LoadConfig will now pull the *resolved* values from Viper's internal state,
			// which includes values set by flags, environment variables, or the .env file.
			cfg = config.LoadConfig(logger)

			b, err := bot.NewTelegramBot(&cfg, logger)
			if err != nil {
				log.Fatalf("Error initializing Telegram bot: %v", err)
			}

			b.Run()
		},
	}

	// 4. Define Cobra flags:
	// Use the pointer to the package-level `cfg` to allow Cobra to populate them.
	// Provide default values directly in the flag definitions.
	rootCmd.Flags().IntVar(&cfg.ApiID, "api_id", 0, "Telegram API ID (required)")
	rootCmd.Flags().StringVar(&cfg.ApiHash, "api_hash", "", "Telegram API Hash (required)")
	rootCmd.Flags().StringVar(&cfg.BotToken, "bot_token", "", "Telegram Bot Token (required)")
	rootCmd.Flags().StringVar(&cfg.BaseURL, "base_url", "", "Base URL for the web interface (required)")
	rootCmd.Flags().StringVar(&cfg.Port, "port", "8080", "Port for the web server (default 8080)")
	rootCmd.Flags().IntVar(&cfg.HashLength, "hash_length", 8, "Length of the short hash for file URLs (default 8)")
	rootCmd.Flags().StringVar(&cfg.CacheDirectory, "cache_directory", ".cache", "Directory to store cached files and database (default .cache)")
	rootCmd.Flags().Int64Var(&cfg.MaxCacheSize, "max_cache_size", 10*1024*1024*1024, "Maximum cache size in bytes (default 10GB)")
	rootCmd.Flags().BoolVar(&cfg.DebugMode, "debug_mode", false, "Enable debug logging (default false)")

	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
