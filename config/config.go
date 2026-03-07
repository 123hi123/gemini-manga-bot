package config

import (
	"os"
)

type Config struct {
	GeminiAPIKey  string
	GeminiBaseURL string
	GeminiModel   string
	BotToken      string
	DataDir       string

	// Grok settings
	GrokAPIKey   string
	GrokBaseURL  string
	GrokImgModel  string // image generation model (default: grok-imagine-1.0)
	GrokEditModel string // image editing model (default: grok-imagine-1.0-edit)
	GrokVideoModel string // video generation model (default: grok-imagine-1.0-video)
}

// 預設的翻譯 Prompt
const DefaultPrompt = "漫画的文本翻譯为中文放置在旁邊輔助學習，保持原文的风格颜色等，其余非文字部分比如元素布局保持不变，原比例输出"

// 擷取文字的 Prompt
const ExtractTextPrompt = "请提取这张漫画图片中的所有文字对话内容，按顺序列出，格式为纯文本，不要加任何额外说明。"

// TTS 設定
const TTSVoiceName = "Kore"

func LoadConfig() *Config {
	return &Config{
		GeminiAPIKey:  getEnv("GEMINI_API_KEY", ""),
		GeminiBaseURL: getEnv("GEMINI_BASE_URL", ""),
		GeminiModel:   getEnv("GEMINI_MODEL", ""),
		BotToken:      getEnv("BOT_TOKEN", ""),
		DataDir:       getEnv("DATA_DIR", "./data"),

		GrokAPIKey:     getEnv("GROK_API_KEY", ""),
		GrokBaseURL:    getEnv("GROK_BASE_URL", "http://127.0.0.1:8000"),
		GrokImgModel:   getEnv("GROK_IMG_MODEL", "grok-imagine-1.0"),
		GrokEditModel:  getEnv("GROK_EDIT_MODEL", "grok-imagine-1.0-edit"),
		GrokVideoModel: getEnv("GROK_VIDEO_MODEL", "grok-imagine-1.0-video"),
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
