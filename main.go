package main

import (
	"log"

	"tg-bawer/bot"
	"tg-bawer/config"
	"tg-bawer/database"
)

func main() {
	// 載入設定
	cfg := config.LoadConfig()

	if cfg.GeminiAPIKey == "" {
		log.Fatal("請設定環境變數 GEMINI_API_KEY")
	}
	if cfg.BotToken == "" {
		log.Fatal("請設定環境變數 BOT_TOKEN")
	}

	// 初始化資料庫
	db, err := database.NewDatabase(cfg.DataDir)
	if err != nil {
		log.Fatalf("無法初始化資料庫: %v", err)
	}
	defer db.Close()

	log.Printf("資料目錄: %s", cfg.DataDir)

	// 建立並啟動 Bot
	b, err := bot.NewBot(cfg, db)
	if err != nil {
		log.Fatalf("無法建立 Bot: %v", err)
	}

	log.Println("Bot 已啟動！")
	b.Run()
}
