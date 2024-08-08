package main

import (
	"fmt"
	"github.com/spf13/cobra"
	"log"
	"os"
	"webBridgeBot/internal/bot"
	"webBridgeBot/internal/config"
)

var cfg config.Configuration

func main() {
	logger := log.New(os.Stdout, "webBridgeBot: ", log.Ldate|log.Ltime|log.Lshortfile)
	rootCmd := &cobra.Command{
		Use:   "webBridgeBot",
		Short: "WebBridgeBot",
		Run: func(cmd *cobra.Command, args []string) {
			cfg = config.LoadConfig(logger)
			b, err := bot.NewTelegramBot(&cfg, logger)
			if err != nil {
				log.Fatalf("Error initializing Telegram bot: %v", err)
			}

			b.Run()
		},
	}

	// Define flags
	defineFlags(rootCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func defineFlags(cmd *cobra.Command) {
	cmd.Flags().IntVar(&cfg.ApiID, "api_id", 0, "API ID")
	cmd.Flags().StringVar(&cfg.ApiHash, "api_hash", "", "API Hash")
	cmd.Flags().StringVar(&cfg.BotToken, "bot_token", "", "Bot Token")
	cmd.Flags().StringVar(&cfg.BaseURL, "base_url", "", "Base URL")
	cmd.Flags().StringVar(&cfg.Port, "port", "", "Port")
	cmd.Flags().IntVar(&cfg.HashLength, "hash_length", 0, "Hash Length")
	cmd.Flags().StringVar(&cfg.CacheDirectory, "cache_directory", "", "Cache Directory")
	cmd.Flags().Int64Var(&cfg.MaxCacheSize, "max_cache_size", 0, "Max Cache Size")
	cmd.Flags().BoolVar(&cfg.DebugMode, "debug_mode", false, "Enable Debug Mode")
}
