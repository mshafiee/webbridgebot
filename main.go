package main

import (
	"fmt"
	"github.com/spf13/cobra"
	"log"
	"os"
	"webBridgeBot/internal/bot"
	"webBridgeBot/internal/config"
)

var (
	cfgFile string
	cfg     config.Configuration
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "telegram-bot",
		Short: "Telegram Bot",
		Run: func(cmd *cobra.Command, args []string) {
			cfg = config.LoadConfig()
			b, err := bot.NewTelegramBot(&cfg)
			if err != nil {
				log.Fatalf("Error initializing Telegram bot: %v", err)
			}

			b.Run()
		},
	}

	rootCmd.Flags().StringVarP(&cfgFile, "cfg", "c", "", "cfg file (default is .env)")

	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
