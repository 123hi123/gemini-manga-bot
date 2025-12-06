# TG-Bawer 🍌✏️

Telegram Bot powered by Gemini - 用 AI 畫你想要的圖！

> **Bawer** = **Ba**nana + Dra**wer** 🎨

## 功能

- 🎨 **圖片翻譯** - 自動將漫畫文字翻譯成中文
- 🔊 **語音朗讀** - 擷取文字並生成 TTS 語音 (使用 `/v` 參數)
- 📝 **Prompt 管理** - 保存、列出、設定預設 Prompt
- 📜 **使用歷史** - 查看過往使用的 Prompt
- ⚙️ **畫質設定** - 支援 1K/2K/4K 畫質
- 🔄 **自動重試** - 失敗時自動降級重試 (2K×3 → 1K×3)

## 快速開始

### 一行部署（Linux）

```bash
docker run -d --name tg-bawer --restart unless-stopped -e GEMINI_API_KEY=你的API_KEY -e BOT_TOKEN=你的BOT_TOKEN -v ~/.tg-bawer:/app/data ghcr.io/123hi123/tg-bawer:latest
```

### 使用 GitHub Container Registry 鏡像

```bash
docker run -d \
  --name tg-bawer \
  --restart unless-stopped \
  -e GEMINI_API_KEY=your_key \
  -e BOT_TOKEN=your_token \
  -v ~/.tg-bawer:/app/data \
  ghcr.io/123hi123/tg-bawer:latest
```

### 使用 Docker Compose（推薦）

1. 複製環境變數範本：
   ```bash
   cp .env.example .env
   ```

2. 編輯 `.env` 填入你的 API Key：
   ```
   GEMINI_API_KEY=your_gemini_api_key
   BOT_TOKEN=your_telegram_bot_token
   ```

3. 啟動：
   ```bash
   docker-compose up -d
   ```

### 使用 Docker

```bash
docker build -t tg-bawer .
docker run -d \
  --name tg-bawer \
  -e GEMINI_API_KEY=your_key \
  -e BOT_TOKEN=your_token \
  -v $(pwd)/data:/app/data \
  tg-bawer
```

### 本地執行

```bash
# 安裝依賴
go mod tidy

# 設定環境變數
export GEMINI_API_KEY=your_key
export BOT_TOKEN=your_token
export DATA_DIR=./data

# 執行
go run .
```

## 使用方式

### 基本用法

| 操作 | 說明 |
|------|------|
| 直接傳圖片 | 使用預設 Prompt 翻譯 |
| 圖片 + 文字 | 使用該文字作為 Prompt |

### 圖片參數

在圖片說明中使用：

| 參數 | 說明 | 範例 |
|------|------|------|
| `/s <畫質>` | 設定畫質 | `/s 4K` |
| `/v` | 同時生成語音（一筆訊息發送圖片+音訊） | `/v` |

組合使用：傳圖片並在說明輸入 `/s 4K /v`

### Bot 指令

| 指令 | 說明 |
|------|------|
| `/start` | 顯示歡迎訊息和使用說明 |
| `/help` | 顯示幫助 |
| `/save <名稱> <prompt>` | 保存 Prompt |
| `/list` | 列出已保存的 Prompt（可點擊複製） |
| `/history` | 查看使用歷史 |
| `/setdefault` | 設定預設 Prompt |
| `/settings` | 設定預設畫質 |
| `/delete` | 刪除已保存的 Prompt |

## 專案結構

```
tg-bawer/
├── main.go              # 程式入口
├── bot/
│   └── bot.go           # Telegram Bot 處理邏輯
├── config/
│   └── config.go        # 設定與常數
├── database/
│   └── database.go      # SQLite 資料庫操作
├── gemini/
│   └── client.go        # Gemini API 客戶端
├── Dockerfile           # Docker 建置檔
├── docker-compose.yml   # Docker Compose 設定
├── .env.example         # 環境變數範本
└── README.md
```

## 資料庫結構

SQLite 資料庫 (`data/bot.db`) 包含：

- `saved_prompts` - 使用者保存的 Prompt
- `prompt_history` - Prompt 使用歷史
- `user_settings` - 使用者設定（預設畫質等）

## 環境變數

| 變數 | 必填 | 說明 |
|------|------|------|
| `GEMINI_API_KEY` | ✅ | Google Gemini API Key |
| `BOT_TOKEN` | ✅ | Telegram Bot Token |
| `DATA_DIR` | ❌ | 資料目錄（預設 `./data`） |

## 技術細節

### 重試邏輯

當圖片生成失敗時：
1. 前 3 次使用使用者設定的畫質（預設 2K）
2. 後 3 次降級為 1K
3. 共最多重試 6 次

### API 呼叫流程

**一般模式：**
1. `gemini-3-pro-image-preview` → 翻譯圖片

**語音模式 (`/v`)：**
1. `gemini-2.5-flash` → 擷取原文
2. `gemini-3-pro-image-preview` → 翻譯圖片
3. `gemini-2.5-flash-preview-tts` → 生成語音

## License

MIT
