package bot

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"tg-bawer/config"
	"tg-bawer/database"
	"tg-bawer/gemini"
	"tg-bawer/grok"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// mediaGroupCache 用於快取 Media Group 的圖片
type mediaGroupCache struct {
	sync.RWMutex
	groups map[string][]cachedImage // key: MediaGroupID
}

type cachedImage struct {
	FileID    string
	Timestamp time.Time
}

type Bot struct {
	api         *tgbotapi.BotAPI
	gemini      *gemini.Client
	grokClient  *grok.Client
	db          *database.Database
	config      *config.Config
	mediaGroups *mediaGroupCache
	imageQueues *userImageQueueCache
}

// userImageQueueCache is an in-memory cache for per-user image queues with expiration.
type userImageQueueCache struct {
	sync.RWMutex
	queues map[string][]queuedImage // key: "userID:chatID"
}

type queuedImage struct {
	FileID    string
	Timestamp time.Time
}

func NewBot(cfg *config.Config, db *database.Database) (*Bot, error) {
	api, err := tgbotapi.NewBotAPI(cfg.BotToken)
	if err != nil {
		return nil, err
	}

	log.Printf("Bot authorized on account %s", api.Self.UserName)

	bot := &Bot{
		api: api,
		gemini: gemini.NewClientWithService(gemini.ServiceConfig{
			Type:    gemini.ServiceTypeStandard,
			Name:    "env-default",
			APIKey:  cfg.GeminiAPIKey,
			BaseURL: cfg.GeminiBaseURL,
			Model:   cfg.GeminiModel,
		}),
		grokClient: grok.NewClient(
			cfg.GrokAPIKey,
			cfg.GrokBaseURL,
			cfg.GrokImgModel,
			cfg.GrokEditModel,
			cfg.GrokVideoModel,
		),
		db:     db,
		config: cfg,
		mediaGroups: &mediaGroupCache{
			groups: make(map[string][]cachedImage),
		},
		imageQueues: &userImageQueueCache{
			queues: make(map[string][]queuedImage),
		},
	}

	// 啟動清理過期快取的 goroutine
	go bot.cleanupMediaGroupCache()
	go bot.retryFailedGenerations()
	go bot.cleanupImageQueues()

	return bot, nil
}

func (b *Bot) Run() {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	u.AllowedUpdates = []string{"message", "callback_query", "message_reaction"}

	updates := b.api.GetUpdatesChan(u)

	for update := range updates {
		if update.Message != nil {
			go b.handleMessage(update.Message)
		} else if update.CallbackQuery != nil {
			go b.handleCallback(update.CallbackQuery)
		}
	}
}

// cleanupMediaGroupCache 定期清理過期的 Media Group 快取
func (b *Bot) cleanupMediaGroupCache() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		b.mediaGroups.Lock()
		now := time.Now()
		for groupID, images := range b.mediaGroups.groups {
			// 檢查第一張圖片的時間（最早的）
			if len(images) > 0 && now.Sub(images[0].Timestamp) > 10*time.Minute {
				delete(b.mediaGroups.groups, groupID)
			}
		}
		b.mediaGroups.Unlock()
	}
}

// cacheMediaGroupImage 快取 Media Group 中的圖片
func (b *Bot) cacheMediaGroupImage(mediaGroupID string, fileID string) {
	b.mediaGroups.Lock()
	defer b.mediaGroups.Unlock()

	b.mediaGroups.groups[mediaGroupID] = append(b.mediaGroups.groups[mediaGroupID], cachedImage{
		FileID:    fileID,
		Timestamp: time.Now(),
	})
	log.Printf("[MediaGroup] 快取圖片: GroupID=%s, FileID=%s, 目前數量=%d",
		mediaGroupID, fileID[:20]+"...", len(b.mediaGroups.groups[mediaGroupID]))
}

// getMediaGroupImages 取得 Media Group 中所有圖片的 FileID
func (b *Bot) getMediaGroupImages(mediaGroupID string) []string {
	b.mediaGroups.RLock()
	defer b.mediaGroups.RUnlock()

	images := b.mediaGroups.groups[mediaGroupID]
	fileIDs := make([]string, len(images))
	for i, img := range images {
		fileIDs[i] = img.FileID
	}
	log.Printf("[MediaGroup] 取得圖片: GroupID=%s, 數量=%d", mediaGroupID, len(fileIDs))
	return fileIDs
}

// cleanupImageQueues 定期清理過期的使用者圖片佇列
func (b *Bot) cleanupImageQueues() {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		b.imageQueues.Lock()
		now := time.Now()
		for key, images := range b.imageQueues.queues {
			var valid []queuedImage
			for _, img := range images {
				if now.Sub(img.Timestamp) < 10*time.Minute {
					valid = append(valid, img)
				}
			}
			if len(valid) == 0 {
				delete(b.imageQueues.queues, key)
			} else {
				b.imageQueues.queues[key] = valid
			}
		}
		b.imageQueues.Unlock()

		// Also clean database
		if err := b.db.CleanExpiredImageQueue(10 * time.Minute); err != nil {
			log.Printf("清理過期圖片佇列失敗: %v", err)
		}
	}
}

// addToImageQueue adds an image to a user's image queue.
func (b *Bot) addToImageQueue(userID, chatID int64, fileID string) {
	b.imageQueues.Lock()
	defer b.imageQueues.Unlock()

	key := fmt.Sprintf("%d:%d", userID, chatID)
	// Check if already in queue
	for _, img := range b.imageQueues.queues[key] {
		if img.FileID == fileID {
			return // Already queued
		}
	}
	b.imageQueues.queues[key] = append(b.imageQueues.queues[key], queuedImage{
		FileID:    fileID,
		Timestamp: time.Now(),
	})
	log.Printf("[ImageQueue] 新增圖片: UserID=%d, ChatID=%d, FileID=%s, 目前數量=%d",
		userID, chatID, fileID[:min(20, len(fileID))]+"...", len(b.imageQueues.queues[key]))

	// Also persist to database
	b.db.AddImageToQueue(userID, chatID, fileID, "")
}

// getUserImageQueue gets and clears a user's image queue.
func (b *Bot) getUserImageQueue(userID, chatID int64) []string {
	b.imageQueues.Lock()
	defer b.imageQueues.Unlock()

	key := fmt.Sprintf("%d:%d", userID, chatID)
	images := b.imageQueues.queues[key]
	delete(b.imageQueues.queues, key)

	fileIDs := make([]string, len(images))
	for i, img := range images {
		fileIDs[i] = img.FileID
	}
	log.Printf("[ImageQueue] 取得並清除佇列: UserID=%d, ChatID=%d, 數量=%d", userID, chatID, len(fileIDs))

	// Also clear from database
	b.db.ClearUserImageQueue(userID, chatID)

	return fileIDs
}

func (b *Bot) handleMessage(msg *tgbotapi.Message) {
	// 處理指令（斜線指令在群組和私聊都生效）
	if msg.IsCommand() {
		b.handleCommand(msg)
		return
	}

	// 判斷是否在群組中
	isGroup := msg.Chat.Type == "group" || msg.Chat.Type == "supergroup"

	// 快取 Media Group 中的圖片
	if len(msg.Photo) > 0 && msg.MediaGroupID != "" {
		photo := msg.Photo[len(msg.Photo)-1]
		b.cacheMediaGroupImage(msg.MediaGroupID, photo.FileID)
		log.Printf("[收到圖片] MediaGroupID=%s, MessageID=%d", msg.MediaGroupID, msg.MessageID)
	} else if len(msg.Photo) > 0 {
		log.Printf("[收到圖片] 單張圖片（無 MediaGroupID）, MessageID=%d", msg.MessageID)
	}

	// 處理圖片回覆文字的情況（用圖片回覆一則文字訊息）
	// 圖片指令在群組和私聊行為相同
	if len(msg.Photo) > 0 && msg.Caption == "" {
		// 檢查是否回覆了一則文字訊息
		if msg.ReplyToMessage != nil && msg.ReplyToMessage.Text != "" {
			b.handleImageReplyText(msg)
			return
		}
		// 單獨傳圖片，不做任何處理
		return
	}

	// 處理貼圖回覆文字的情況（用貼圖回覆一則文字訊息）
	// 貼圖指令在群組和私聊行為相同
	if msg.Sticker != nil {
		// 檢查是否回覆了一則文字訊息
		if msg.ReplyToMessage != nil && msg.ReplyToMessage.Text != "" {
			b.handleStickerReplyText(msg)
			return
		}
		// 單獨傳貼圖，不做任何處理
		return
	}

	// 處理文字訊息（非指令）
	if msg.Text != "" {
		// 在群組中，文字訊息必須以 . 開頭才會處理
		if isGroup {
			if !strings.HasPrefix(msg.Text, ".") {
				return // 群組中不以 . 開頭的訊息，忽略
			}
		}
		b.handleTextMessage(msg)
		return
	}

	// 處理帶有 caption 的圖片
	if len(msg.Photo) > 0 && msg.Caption != "" {
		// 在群組中，caption 必須以 . 開頭才會處理
		if isGroup {
			if !strings.HasPrefix(msg.Caption, ".") {
				return // 群組中不以 . 開頭的訊息，忽略
			}
		}
		b.handleTextMessage(msg)
		return
	}
}

func (b *Bot) handleCommand(msg *tgbotapi.Message) {
	switch msg.Command() {
	case "start":
		b.cmdStart(msg)
	case "help":
		b.cmdHelp(msg)
	case "save":
		b.cmdSave(msg)
	case "list":
		b.cmdList(msg)
	case "history":
		b.cmdHistory(msg)
	case "setdefault":
		b.cmdSetDefault(msg)
	case "settings":
		b.cmdSettings(msg)
	case "delete":
		b.cmdDelete(msg)
	case "service":
		b.cmdService(msg)
	case "queue":
		b.cmdQueue(msg)
	}
}

func (b *Bot) cmdStart(msg *tgbotapi.Message) {
	text := `�✏️ *TG-Bawer*

用 AI 畫你想要的圖！

*基本用法：*
• 直接輸入文字 → AI 根據描述生成圖片
• 回覆圖片/貼圖並輸入文字 → AI 根據圖片進行編輯
• 回覆文字並傳圖片/貼圖 → 同上，另一種操作方式
• 上傳多張圖片後回覆其一 → AI 會抓取所有圖片處理

*群組使用：*
在群組中，文字訊息需以 ` + "`.`" + ` 開頭才會觸發
例如：` + "`.幫我畫一隻貓 @16:9`" + `

*參數設定（用 @ 符號，前後需有空格）：*
• ` + "`@1:1`" + ` ` + "`@16:9`" + ` ` + "`@9:16`" + ` → 設定比例
• ` + "`@4K`" + ` ` + "`@2K`" + ` ` + "`@1K`" + ` → 設定畫質
• ` + "`@s`" + ` → 回覆群組圖片時只使用單張，不抓整組

*支援的比例：*
` + "`@1:1`" + ` ` + "`@2:3`" + ` ` + "`@3:2`" + ` ` + "`@3:4`" + ` ` + "`@4:3`" + ` ` + "`@4:5`" + ` ` + "`@5:4`" + ` ` + "`@9:16`" + ` ` + "`@16:9`" + ` ` + "`@21:9`" + `

💡 不指定比例時：
• 有圖片時，使用最接近原圖的支援比例
• 沒有圖片時，預設使用 1:1

*範例：*
` + "`畫一隻可愛的貓咪 @16:9 @4K`" + `

*指令：*
/save <名稱> <prompt> - 保存 Prompt
/list - 列出已保存的 Prompt
/history - 查看使用歷史
/setdefault - 設定預設 Prompt
/settings - 設定預設畫質
/delete - 刪除已保存的 Prompt
/service - 服務管理（standard/custom/vertex）
/queue - 查看待重試任務佇列
/help - 顯示幫助`

	reply := tgbotapi.NewMessage(msg.Chat.ID, text)
	reply.ParseMode = "Markdown"
	b.api.Send(reply)
}

func (b *Bot) cmdHelp(msg *tgbotapi.Message) {
	b.cmdStart(msg)
}

func (b *Bot) cmdSave(msg *tgbotapi.Message) {
	args := msg.CommandArguments()
	parts := strings.SplitN(args, " ", 2)
	if len(parts) < 2 {
		reply := tgbotapi.NewMessage(msg.Chat.ID, "❌ 格式：/save <名稱> <prompt>\n例如：/save 學習模式 漫画的文本翻譯为中文...")
		b.api.Send(reply)
		return
	}

	name := parts[0]
	prompt := parts[1]

	if err := b.db.SavePrompt(msg.From.ID, name, prompt); err != nil {
		reply := tgbotapi.NewMessage(msg.Chat.ID, "❌ 保存失敗："+err.Error())
		b.api.Send(reply)
		return
	}

	reply := tgbotapi.NewMessage(msg.Chat.ID, fmt.Sprintf("✅ 已保存 Prompt「%s」", name))
	b.api.Send(reply)
}

func (b *Bot) cmdList(msg *tgbotapi.Message) {
	prompts, err := b.db.GetSavedPrompts(msg.From.ID)
	if err != nil {
		reply := tgbotapi.NewMessage(msg.Chat.ID, "❌ 取得失敗："+err.Error())
		b.api.Send(reply)
		return
	}

	if len(prompts) == 0 {
		reply := tgbotapi.NewMessage(msg.Chat.ID, "📝 尚未保存任何 Prompt\n使用 /save <名稱> <prompt> 來保存")
		b.api.Send(reply)
		return
	}

	var rows [][]tgbotapi.InlineKeyboardButton
	for _, p := range prompts {
		defaultMark := ""
		if p.IsDefault {
			defaultMark = " ⭐"
		}
		btn := tgbotapi.NewInlineKeyboardButtonData(
			fmt.Sprintf("%s%s", p.Name, defaultMark),
			fmt.Sprintf("copy:%d", p.ID),
		)
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(btn))
	}

	keyboard := tgbotapi.NewInlineKeyboardMarkup(rows...)
	reply := tgbotapi.NewMessage(msg.Chat.ID, "📋 *已保存的 Prompt*\n點擊可複製內容：")
	reply.ParseMode = "Markdown"
	reply.ReplyMarkup = keyboard
	b.api.Send(reply)
}

func (b *Bot) cmdHistory(msg *tgbotapi.Message) {
	history, err := b.db.GetHistory(msg.From.ID, 10)
	if err != nil {
		reply := tgbotapi.NewMessage(msg.Chat.ID, "❌ 取得失敗："+err.Error())
		b.api.Send(reply)
		return
	}

	if len(history) == 0 {
		reply := tgbotapi.NewMessage(msg.Chat.ID, "📜 尚無使用記錄")
		b.api.Send(reply)
		return
	}

	var rows [][]tgbotapi.InlineKeyboardButton
	for i, h := range history {
		preview := h.Prompt
		if len(preview) > 30 {
			preview = preview[:30] + "..."
		}
		btn := tgbotapi.NewInlineKeyboardButtonData(
			fmt.Sprintf("%d. %s", i+1, preview),
			fmt.Sprintf("hist:%d", h.ID),
		)
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(btn))
	}

	keyboard := tgbotapi.NewInlineKeyboardMarkup(rows...)
	reply := tgbotapi.NewMessage(msg.Chat.ID, "📜 *最近使用的 Prompt*\n點擊可複製：")
	reply.ParseMode = "Markdown"
	reply.ReplyMarkup = keyboard
	b.api.Send(reply)
}

func (b *Bot) cmdSetDefault(msg *tgbotapi.Message) {
	prompts, err := b.db.GetSavedPrompts(msg.From.ID)
	if err != nil {
		reply := tgbotapi.NewMessage(msg.Chat.ID, "❌ 取得失敗："+err.Error())
		b.api.Send(reply)
		return
	}

	if len(prompts) == 0 {
		reply := tgbotapi.NewMessage(msg.Chat.ID, "📝 尚未保存任何 Prompt\n先使用 /save 保存後再設定預設")
		b.api.Send(reply)
		return
	}

	var rows [][]tgbotapi.InlineKeyboardButton
	for _, p := range prompts {
		mark := "○"
		if p.IsDefault {
			mark = "●"
		}
		btn := tgbotapi.NewInlineKeyboardButtonData(
			fmt.Sprintf("%s %s", mark, p.Name),
			fmt.Sprintf("default:%d", p.ID),
		)
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(btn))
	}

	keyboard := tgbotapi.NewInlineKeyboardMarkup(rows...)
	reply := tgbotapi.NewMessage(msg.Chat.ID, "⭐ *選擇預設 Prompt*：")
	reply.ParseMode = "Markdown"
	reply.ReplyMarkup = keyboard
	b.api.Send(reply)
}

func (b *Bot) cmdSettings(msg *tgbotapi.Message) {
	currentQuality, _ := b.db.GetUserSettings(msg.From.ID)

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(qualityButton("1K", currentQuality), "quality:1K"),
			tgbotapi.NewInlineKeyboardButtonData(qualityButton("2K", currentQuality), "quality:2K"),
			tgbotapi.NewInlineKeyboardButtonData(qualityButton("4K", currentQuality), "quality:4K"),
		),
	)

	reply := tgbotapi.NewMessage(msg.Chat.ID, fmt.Sprintf("⚙️ *設定*\n\n目前預設畫質：*%s*\n\n點擊更改：", currentQuality))
	reply.ParseMode = "Markdown"
	reply.ReplyMarkup = keyboard
	b.api.Send(reply)
}

func qualityButton(q, current string) string {
	if q == current {
		return "● " + q
	}
	return "○ " + q
}

func (b *Bot) cmdDelete(msg *tgbotapi.Message) {
	prompts, err := b.db.GetSavedPrompts(msg.From.ID)
	if err != nil || len(prompts) == 0 {
		reply := tgbotapi.NewMessage(msg.Chat.ID, "📝 沒有可刪除的 Prompt")
		b.api.Send(reply)
		return
	}

	var rows [][]tgbotapi.InlineKeyboardButton
	for _, p := range prompts {
		btn := tgbotapi.NewInlineKeyboardButtonData(
			fmt.Sprintf("🗑 %s", p.Name),
			fmt.Sprintf("del:%d", p.ID),
		)
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(btn))
	}

	keyboard := tgbotapi.NewInlineKeyboardMarkup(rows...)
	reply := tgbotapi.NewMessage(msg.Chat.ID, "🗑 *選擇要刪除的 Prompt*：")
	reply.ParseMode = "Markdown"
	reply.ReplyMarkup = keyboard
	b.api.Send(reply)
}

func (b *Bot) cmdQueue(msg *tgbotapi.Message) {
	counts, err := b.db.GetFailedGenerationCounts(msg.From.ID)
	if err != nil {
		reply := tgbotapi.NewMessage(msg.Chat.ID, "❌ 取得佇列失敗："+err.Error())
		b.api.Send(reply)
		return
	}

	// Consolidate backward-compatible source values into canonical task types
	googleCount := counts[taskTypeGoogleImage] + counts["google"]
	grokImgCount := counts[taskTypeGrokImage] + counts["grok"]
	grokVideoCount := counts[taskTypeGrokVideo]
	total := googleCount + grokImgCount + grokVideoCount

	var text string
	if total == 0 {
		text = "✅ 目前沒有待重試的任務"
	} else {
		text = fmt.Sprintf(
			"📋 *待重試任務列表*\n\n🌐 Google 圖片：%d\n🤖 Grok 圖片：%d\n🎬 Grok 影片：%d\n\n總計：%d",
			googleCount, grokImgCount, grokVideoCount, total,
		)
	}

	reply := tgbotapi.NewMessage(msg.Chat.ID, text)
	reply.ParseMode = "Markdown"
	b.api.Send(reply)
}

func (b *Bot) handleCallback(callback *tgbotapi.CallbackQuery) {
	data := callback.Data
	parts := strings.SplitN(data, ":", 2)
	if len(parts) != 2 {
		return
	}

	action := parts[0]
	value := parts[1]

	switch action {
	case "copy":
		b.callbackCopy(callback, value)
	case "hist":
		b.callbackHistory(callback, value)
	case "default":
		b.callbackDefault(callback, value)
	case "quality":
		b.callbackQuality(callback, value)
	case "del":
		b.callbackDelete(callback, value)
	}
}

func (b *Bot) callbackCopy(callback *tgbotapi.CallbackQuery, idStr string) {
	var id int64
	fmt.Sscanf(idStr, "%d", &id)

	prompts, _ := b.db.GetSavedPrompts(callback.From.ID)
	for _, p := range prompts {
		if p.ID == id {
			// 發送 Prompt 內容讓使用者複製
			reply := tgbotapi.NewMessage(callback.Message.Chat.ID, fmt.Sprintf("📋 *%s*\n\n`%s`", p.Name, p.Prompt))
			reply.ParseMode = "Markdown"
			b.api.Send(reply)
			break
		}
	}

	b.api.Request(tgbotapi.NewCallback(callback.ID, "已顯示 Prompt 內容"))
}

func (b *Bot) callbackHistory(callback *tgbotapi.CallbackQuery, idStr string) {
	var id int64
	fmt.Sscanf(idStr, "%d", &id)

	history, _ := b.db.GetHistory(callback.From.ID, 100)
	for _, h := range history {
		if h.ID == id {
			reply := tgbotapi.NewMessage(callback.Message.Chat.ID, fmt.Sprintf("📜 *歷史 Prompt*\n\n`%s`", h.Prompt))
			reply.ParseMode = "Markdown"
			b.api.Send(reply)
			break
		}
	}

	b.api.Request(tgbotapi.NewCallback(callback.ID, ""))
}

func (b *Bot) callbackDefault(callback *tgbotapi.CallbackQuery, idStr string) {
	var id int64
	fmt.Sscanf(idStr, "%d", &id)

	if err := b.db.SetDefaultPrompt(callback.From.ID, id); err != nil {
		b.api.Request(tgbotapi.NewCallback(callback.ID, "設定失敗"))
		return
	}

	b.api.Request(tgbotapi.NewCallback(callback.ID, "✅ 已設定為預設"))

	// 重新顯示列表
	b.cmdSetDefault(callback.Message)
}

func (b *Bot) callbackQuality(callback *tgbotapi.CallbackQuery, quality string) {
	if err := b.db.SetUserSettings(callback.From.ID, quality); err != nil {
		b.api.Request(tgbotapi.NewCallback(callback.ID, "設定失敗"))
		return
	}

	b.api.Request(tgbotapi.NewCallback(callback.ID, fmt.Sprintf("✅ 預設畫質已設為 %s", quality)))

	// 更新訊息
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(qualityButton("1K", quality), "quality:1K"),
			tgbotapi.NewInlineKeyboardButtonData(qualityButton("2K", quality), "quality:2K"),
			tgbotapi.NewInlineKeyboardButtonData(qualityButton("4K", quality), "quality:4K"),
		),
	)

	edit := tgbotapi.NewEditMessageText(callback.Message.Chat.ID, callback.Message.MessageID,
		fmt.Sprintf("⚙️ *設定*\n\n目前預設畫質：*%s*\n\n點擊更改：", quality))
	edit.ParseMode = "Markdown"
	edit.ReplyMarkup = &keyboard
	b.api.Send(edit)
}

func (b *Bot) callbackDelete(callback *tgbotapi.CallbackQuery, idStr string) {
	var id int64
	fmt.Sscanf(idStr, "%d", &id)

	if err := b.db.DeletePrompt(callback.From.ID, id); err != nil {
		b.api.Request(tgbotapi.NewCallback(callback.ID, "刪除失敗"))
		return
	}

	b.api.Request(tgbotapi.NewCallback(callback.ID, "✅ 已刪除"))

	// 重新顯示列表
	b.cmdDelete(callback.Message)
}

// 支援的比例列表
var supportedRatios = map[string]bool{
	"1:1": true, "2:3": true, "3:2": true,
	"3:4": true, "4:3": true, "4:5": true,
	"5:4": true, "9:16": true, "16:9": true,
	"21:9": true,
}

// 支援的畫質列表
var supportedQualities = map[string]string{
	"1k": "1K", "2k": "2K", "4k": "4K",
	"1K": "1K", "2K": "2K", "4K": "4K",
}

// ParsedParams 解析後的參數
type ParsedParams struct {
	Prompt               string
	AspectRatio          string // 如果沒指定則為空
	Quality              string // 如果沒指定則為空
	SingleImageFromGroup bool   // @s：回覆群組圖時只取單張
	RatioError           string // 比例錯誤訊息
	QualityError         string // 畫質錯誤訊息
}

// parseTextParams 解析文字中的 @ 參數
func parseTextParams(text string) *ParsedParams {
	params := &ParsedParams{}

	// 用空格分割
	parts := strings.Fields(text)
	var promptParts []string

	for _, part := range parts {
		if strings.HasPrefix(part, "@") {
			value := strings.TrimPrefix(part, "@")
			lowerValue := strings.ToLower(value)

			// 群組圖模式：只取單張
			if lowerValue == "s" {
				params.SingleImageFromGroup = true
				continue
			}

			// 檢查是否為畫質
			if q, ok := supportedQualities[value]; ok {
				params.Quality = q
				continue
			}

			// 檢查是否為比例
			if supportedRatios[value] {
				params.AspectRatio = value
				continue
			}

			// 檢查是否為無效的畫質格式 (數字+K)
			upperValue := strings.ToUpper(value)
			if strings.HasSuffix(upperValue, "K") && len(value) > 1 {
				params.QualityError = value
				continue
			}

			// 檢查是否為無效的比例格式 (包含冒號)
			if strings.Contains(value, ":") {
				params.RatioError = value
				continue
			}

			// 其他情況視為 prompt 的一部分
			promptParts = append(promptParts, part)
		} else {
			promptParts = append(promptParts, part)
		}
	}

	params.Prompt = strings.Join(promptParts, " ")
	return params
}

// truncateError 截斷錯誤訊息並折疊顯示
func truncateError(err string) string {
	const maxLen = 200
	if len(err) > maxLen {
		return err[:maxLen] + "...\n(錯誤訊息過長已截斷)"
	}
	return err
}

func (b *Bot) handleTextMessage(msg *tgbotapi.Message) {
	// 取得文字內容
	text := msg.Text
	if text == "" {
		text = msg.Caption
	}

	// 判斷是否在群組中，如果是則移除開頭的 .
	isGroup := msg.Chat.Type == "group" || msg.Chat.Type == "supergroup"
	if isGroup && strings.HasPrefix(text, ".") {
		text = strings.TrimPrefix(text, ".")
		text = strings.TrimSpace(text) // 移除前導空白
	}

	// 如果是斜線開頭但不是指令（例如不正確的格式），跳過
	if strings.HasPrefix(text, "/") {
		return
	}

	// 解析參數
	params := parseTextParams(text)

	// 檢查參數錯誤
	if params.RatioError != "" || params.QualityError != "" {
		errorText := "❌ *參數錯誤*\n\n"

		if params.RatioError != "" {
			errorText += fmt.Sprintf("無效的比例：`%s`\n", params.RatioError)
			errorText += "支援的比例：`@1:1` `@2:3` `@3:2` `@3:4` `@4:3` `@4:5` `@5:4` `@9:16` `@16:9` `@21:9`\n\n"
		}

		if params.QualityError != "" {
			errorText += fmt.Sprintf("無效的畫質：`%s`\n", params.QualityError)
			errorText += "支援的畫質：`@1K` `@2K` `@4K`\n\n"
		}

		errorText += "*正確範例：*\n`翻譯這張漫畫 @16:9 @4K`"

		reply := tgbotapi.NewMessage(msg.Chat.ID, errorText)
		reply.ParseMode = "Markdown"
		reply.ReplyToMessageID = msg.MessageID
		b.api.Send(reply)
		return
	}

	allServices, _ := b.resolveAllServiceConfigs(msg.From.ID)
	if len(allServices) == 0 && !b.grokClient.Available() {
		reply := tgbotapi.NewMessage(msg.Chat.ID, "❌ 尚未設定任何服務，請先使用 /service add 新增服務或設定 GROK_API_KEY")
		reply.ReplyToMessageID = msg.MessageID
		b.api.Send(reply)
		return
	}

	// 收集圖片
	var images []imageData

	// 收集使用者圖片佇列中的圖片
	queuedFileIDs := b.getUserImageQueue(msg.From.ID, msg.Chat.ID)
	for _, fileID := range queuedFileIDs {
		images = append(images, imageData{FileID: fileID})
	}

	// 檢查當前訊息是否有圖片
	if len(msg.Photo) > 0 {
		photo := msg.Photo[len(msg.Photo)-1]
		images = append(images, imageData{FileID: photo.FileID})
	}

	// 檢查回覆的訊息是否有圖片或貼圖
	if msg.ReplyToMessage != nil {
		replyMsg := msg.ReplyToMessage

		// 回覆的訊息是圖片
		if len(replyMsg.Photo) > 0 {
			// 檢查是否屬於 Media Group
			log.Printf("[回覆圖片] ReplyToMessage MediaGroupID='%s', MessageID=%d", replyMsg.MediaGroupID, replyMsg.MessageID)
			if replyMsg.MediaGroupID != "" {
				if params.SingleImageFromGroup {
					log.Printf("[回覆圖片] 偵測到 @s，僅使用單張圖片")
					photo := replyMsg.Photo[len(replyMsg.Photo)-1]
					images = append(images, imageData{FileID: photo.FileID})
				} else {
					// 等待一小段時間讓所有圖片都被快取（Telegram 會分批發送 Media Group）
					time.Sleep(500 * time.Millisecond)

					// 從快取中取得該 Media Group 的所有圖片
					groupImages := b.getMediaGroupImages(replyMsg.MediaGroupID)
					log.Printf("[回覆圖片] 從快取取得 %d 張圖片", len(groupImages))
					if len(groupImages) > 0 {
						for _, fileID := range groupImages {
							images = append(images, imageData{FileID: fileID})
						}
					} else {
						// 快取中沒有，使用回覆訊息中的圖片
						log.Printf("[回覆圖片] 快取為空，使用單張圖片（圖片可能是在 Bot 啟動前上傳的）")
						photo := replyMsg.Photo[len(replyMsg.Photo)-1]
						images = append(images, imageData{FileID: photo.FileID})
					}
				}
			} else {
				// 單張圖片
				log.Printf("[回覆圖片] 單張圖片（無 MediaGroupID）")
				photo := replyMsg.Photo[len(replyMsg.Photo)-1]
				images = append(images, imageData{FileID: photo.FileID})
			}
		}

		// 回覆的訊息是貼圖
		if replyMsg.Sticker != nil {
			// 優先使用 PNG 縮圖，如果沒有則使用原始貼圖
			if replyMsg.Sticker.Thumbnail != nil {
				images = append(images, imageData{FileID: replyMsg.Sticker.Thumbnail.FileID})
			} else {
				images = append(images, imageData{FileID: replyMsg.Sticker.FileID})
			}
		}

		// 回覆的訊息是文件（可能是圖片檔案）
		if replyMsg.Document != nil {
			mimeType := replyMsg.Document.MimeType
			if strings.HasPrefix(mimeType, "image/") {
				images = append(images, imageData{FileID: replyMsg.Document.FileID})
			}
		}
	}

	// 取得預設設定
	quality := params.Quality
	if quality == "" {
		quality, _ = b.db.GetUserSettings(msg.From.ID)
		if quality == "" {
			quality = "2K"
		}
	}

	aspectRatio := params.AspectRatio

	// 決定使用的 Prompt
	prompt := params.Prompt
	if prompt == "" {
		// 檢查是否有使用者設定的預設
		defaultPrompt, _ := b.db.GetDefaultPrompt(msg.From.ID)
		if defaultPrompt != nil {
			prompt = defaultPrompt.Prompt
		} else {
			prompt = config.DefaultPrompt
		}
	} else {
		// 記錄到歷史
		b.db.AddToHistory(msg.From.ID, prompt)
	}

	// 顯示參數資訊
	ratioDisplay := "Auto"
	if aspectRatio != "" {
		ratioDisplay = aspectRatio
	} else if len(images) == 0 {
		ratioDisplay = defaultAspectRatio + " (預設)"
	}

	qualityDisplay := quality
	if params.Quality == "" {
		qualityDisplay = quality + " (預設)"
	}

	// 發送處理中訊息（回覆使用者的訊息）
	statusText := fmt.Sprintf("⏳ *處理中...*\n\n📏 比例：`%s`\n🎨 畫質：`%s`\n📸 圖片數量：%d",
		ratioDisplay, qualityDisplay, len(images))

	processingMsg, err := b.sendReplyMessage(msg, statusText)
	if err != nil {
		return
	}

	// 下載所有圖片
	var downloadedImages []gemini.DownloadedImage
	for i, img := range images {
		b.updateMessageMarkdown(processingMsg, fmt.Sprintf("⏳ *處理中...*\n\n📏 比例：`%s`\n🎨 畫質：`%s`\n📸 下載圖片 %d/%d...",
			ratioDisplay, qualityDisplay, i+1, len(images)))

		fileConfig := tgbotapi.FileConfig{FileID: img.FileID}
		file, err := b.api.GetFile(fileConfig)
		if err != nil {
			b.updateMessageHTML(processingMsg, fmt.Sprintf("❌ <b>處理失敗</b>\n\n無法取得圖片 %d\n\n<blockquote expandable>%s</blockquote>",
				i+1, truncateError(err.Error())))
			return
		}

		data, mimeType, err := b.downloadFile(file.FilePath)
		if err != nil {
			b.updateMessageHTML(processingMsg, fmt.Sprintf("❌ <b>處理失敗</b>\n\n下載圖片 %d 失敗\n\n<blockquote expandable>%s</blockquote>",
				i+1, truncateError(err.Error())))
			return
		}

		downloadedImages = append(downloadedImages, gemini.DownloadedImage{
			Data:     data,
			MimeType: mimeType,
		})
	}

	// 比例規則：
	// 1. 使用者有指定 -> 使用指定值
	// 2. 有圖片但未指定 -> 使用最接近圖片比例的支援比例
	// 3. 沒圖片且未指定 -> 預設 1:1
	aspectRatio = resolveAspectRatio(params.AspectRatio, downloadedImages)
	ratioDisplay = ratioDisplayText(params.AspectRatio, aspectRatio, len(downloadedImages))

	b.updateMessageMarkdown(processingMsg, fmt.Sprintf("⏳ *同時生成中...*\n\n📏 比例：`%s`\n🎨 畫質：`%s`\n📸 圖片數量：%d",
		ratioDisplay, qualityDisplay, len(images)))

	// 收集圖片 FileID 供重試佇列使用
	var imageFileIDs []string
	for _, img := range images {
		imageFileIDs = append(imageFileIDs, img.FileID)
	}

	b.runAllGenerationTasks(msg, msg.MessageID, prompt, quality, aspectRatio, downloadedImages, imageFileIDs, allServices, processingMsg.MessageID)
}

// handleImageReplyText 處理用圖片回覆文字訊息的情況
func (b *Bot) handleImageReplyText(msg *tgbotapi.Message) {
	// 從被回覆的訊息取得文字
	replyText := msg.ReplyToMessage.Text

	// 如果是斜線開頭，跳過
	if strings.HasPrefix(replyText, "/") {
		return
	}

	// 解析參數（從被回覆的文字中）
	params := parseTextParams(replyText)

	// 檢查參數錯誤
	if params.RatioError != "" || params.QualityError != "" {
		errorText := "❌ *參數錯誤*\n\n"

		if params.RatioError != "" {
			errorText += fmt.Sprintf("無效的比例：`%s`\n", params.RatioError)
			errorText += "支援的比例：`@1:1` `@2:3` `@3:2` `@3:4` `@4:3` `@4:5` `@5:4` `@9:16` `@16:9` `@21:9`\n\n"
		}

		if params.QualityError != "" {
			errorText += fmt.Sprintf("無效的畫質：`%s`\n", params.QualityError)
			errorText += "支援的畫質：`@1K` `@2K` `@4K`\n\n"
		}

		errorText += "*正確範例：*\n`翻譯這張漫畫 @16:9 @4K`"

		reply := tgbotapi.NewMessage(msg.Chat.ID, errorText)
		reply.ParseMode = "Markdown"
		reply.ReplyToMessageID = msg.MessageID
		b.api.Send(reply)
		return
	}

	allServices, _ := b.resolveAllServiceConfigs(msg.From.ID)
	if len(allServices) == 0 && !b.grokClient.Available() {
		reply := tgbotapi.NewMessage(msg.Chat.ID, "❌ 尚未設定任何服務，請先使用 /service add 新增服務或設定 GROK_API_KEY")
		reply.ReplyToMessageID = msg.MessageID
		b.api.Send(reply)
		return
	}

	// 收集圖片（從當前訊息）
	var images []imageData
	if len(msg.Photo) > 0 {
		// 檢查是否屬於 Media Group
		if msg.MediaGroupID != "" {
			if params.SingleImageFromGroup {
				log.Printf("[圖片回覆文字] 偵測到 @s，僅使用單張圖片")
				photo := msg.Photo[len(msg.Photo)-1]
				images = append(images, imageData{FileID: photo.FileID})
			} else {
				// 從快取中取得該 Media Group 的所有圖片
				groupImages := b.getMediaGroupImages(msg.MediaGroupID)
				if len(groupImages) > 0 {
					for _, fileID := range groupImages {
						images = append(images, imageData{FileID: fileID})
					}
				} else {
					// 快取中沒有，使用當前訊息中的圖片
					photo := msg.Photo[len(msg.Photo)-1]
					images = append(images, imageData{FileID: photo.FileID})
				}
			}
		} else {
			// 單張圖片
			photo := msg.Photo[len(msg.Photo)-1]
			images = append(images, imageData{FileID: photo.FileID})
		}
	}

	// 取得預設設定
	quality := params.Quality
	if quality == "" {
		quality, _ = b.db.GetUserSettings(msg.From.ID)
		if quality == "" {
			quality = "2K"
		}
	}

	aspectRatio := params.AspectRatio

	// 決定使用的 Prompt
	prompt := params.Prompt
	if prompt == "" {
		defaultPrompt, _ := b.db.GetDefaultPrompt(msg.From.ID)
		if defaultPrompt != nil {
			prompt = defaultPrompt.Prompt
		} else {
			prompt = config.DefaultPrompt
		}
	} else {
		b.db.AddToHistory(msg.From.ID, prompt)
	}

	// 顯示參數資訊
	ratioDisplay := "Auto"
	if aspectRatio != "" {
		ratioDisplay = aspectRatio
	}

	qualityDisplay := quality
	if params.Quality == "" {
		qualityDisplay = quality + " (預設)"
	}

	// 發送處理中訊息（回覆被引用的文字訊息）
	statusText := fmt.Sprintf("⏳ *處理中...*\n\n📏 比例：`%s`\n🎨 畫質：`%s`\n📸 圖片數量：%d",
		ratioDisplay, qualityDisplay, len(images))

	processingMsg, err := b.sendReplyToMessage(msg.ReplyToMessage, statusText)
	if err != nil {
		return
	}

	// 下載圖片
	var downloadedImages []gemini.DownloadedImage
	for i, img := range images {
		b.updateMessageMarkdown(processingMsg, fmt.Sprintf("⏳ *處理中...*\n\n📏 比例：`%s`\n🎨 畫質：`%s`\n📸 下載圖片 %d/%d...",
			ratioDisplay, qualityDisplay, i+1, len(images)))

		fileConfig := tgbotapi.FileConfig{FileID: img.FileID}
		file, err := b.api.GetFile(fileConfig)
		if err != nil {
			b.updateMessageHTML(processingMsg, fmt.Sprintf("❌ <b>處理失敗</b>\n\n無法取得圖片 %d\n\n<blockquote expandable>%s</blockquote>",
				i+1, truncateError(err.Error())))
			return
		}

		data, mimeType, err := b.downloadFile(file.FilePath)
		if err != nil {
			b.updateMessageHTML(processingMsg, fmt.Sprintf("❌ <b>處理失敗</b>\n\n下載圖片 %d 失敗\n\n<blockquote expandable>%s</blockquote>",
				i+1, truncateError(err.Error())))
			return
		}

		downloadedImages = append(downloadedImages, gemini.DownloadedImage{
			Data:     data,
			MimeType: mimeType,
		})
	}

	// 比例規則：
	// 1. 使用者有指定 -> 使用指定值
	// 2. 有圖片但未指定 -> 使用最接近圖片比例的支援比例
	// 3. 沒圖片且未指定 -> 預設 1:1
	aspectRatio = resolveAspectRatio(params.AspectRatio, downloadedImages)
	ratioDisplay = ratioDisplayText(params.AspectRatio, aspectRatio, len(downloadedImages))

	b.updateMessageMarkdown(processingMsg, fmt.Sprintf("⏳ *同時生成中...*\n\n📏 比例：`%s`\n🎨 畫質：`%s`\n📸 圖片數量：%d",
		ratioDisplay, qualityDisplay, len(images)))

	// 收集圖片 FileID 供重試佇列使用
	var imageFileIDs []string
	for _, img := range images {
		imageFileIDs = append(imageFileIDs, img.FileID)
	}

	b.runAllGenerationTasks(msg, msg.ReplyToMessage.MessageID, prompt, quality, aspectRatio, downloadedImages, imageFileIDs, allServices, processingMsg.MessageID)
}

// handleStickerReplyText 處理用貼圖回覆文字訊息的情況
func (b *Bot) handleStickerReplyText(msg *tgbotapi.Message) {
	// 從被回覆的訊息取得文字
	replyText := msg.ReplyToMessage.Text

	// 如果是斜線開頭，跳過
	if strings.HasPrefix(replyText, "/") {
		return
	}

	// 解析參數（從被回覆的文字中）
	params := parseTextParams(replyText)

	// 檢查參數錯誤
	if params.RatioError != "" || params.QualityError != "" {
		errorText := "❌ *參數錯誤*\n\n"

		if params.RatioError != "" {
			errorText += fmt.Sprintf("無效的比例：`%s`\n", params.RatioError)
			errorText += "支援的比例：`@1:1` `@2:3` `@3:2` `@3:4` `@4:3` `@4:5` `@5:4` `@9:16` `@16:9` `@21:9`\n\n"
		}

		if params.QualityError != "" {
			errorText += fmt.Sprintf("無效的畫質：`%s`\n", params.QualityError)
			errorText += "支援的畫質：`@1K` `@2K` `@4K`\n\n"
		}

		errorText += "*正確範例：*\n`翻譯這張漫畫 @16:9 @4K`"

		reply := tgbotapi.NewMessage(msg.Chat.ID, errorText)
		reply.ParseMode = "Markdown"
		reply.ReplyToMessageID = msg.MessageID
		b.api.Send(reply)
		return
	}

	allServices, _ := b.resolveAllServiceConfigs(msg.From.ID)
	if len(allServices) == 0 && !b.grokClient.Available() {
		reply := tgbotapi.NewMessage(msg.Chat.ID, "❌ 尚未設定任何服務，請先使用 /service add 新增服務或設定 GROK_API_KEY")
		reply.ReplyToMessageID = msg.MessageID
		b.api.Send(reply)
		return
	}

	// 收集貼圖
	var images []imageData
	if msg.Sticker != nil {
		// 優先使用 PNG 縮圖，如果沒有則使用原始貼圖
		if msg.Sticker.Thumbnail != nil {
			images = append(images, imageData{FileID: msg.Sticker.Thumbnail.FileID})
		} else {
			images = append(images, imageData{FileID: msg.Sticker.FileID})
		}
	}

	// 取得預設設定
	quality := params.Quality
	if quality == "" {
		quality, _ = b.db.GetUserSettings(msg.From.ID)
		if quality == "" {
			quality = "2K"
		}
	}

	aspectRatio := params.AspectRatio

	// 決定使用的 Prompt
	prompt := params.Prompt
	if prompt == "" {
		defaultPrompt, _ := b.db.GetDefaultPrompt(msg.From.ID)
		if defaultPrompt != nil {
			prompt = defaultPrompt.Prompt
		} else {
			prompt = config.DefaultPrompt
		}
	} else {
		b.db.AddToHistory(msg.From.ID, prompt)
	}

	// 顯示參數資訊
	ratioDisplay := "Auto"
	if aspectRatio != "" {
		ratioDisplay = aspectRatio
	}

	qualityDisplay := quality
	if params.Quality == "" {
		qualityDisplay = quality + " (預設)"
	}

	// 發送處理中訊息（回覆被引用的文字訊息）
	statusText := fmt.Sprintf("⏳ *處理中...*\n\n📏 比例：`%s`\n🎨 畫質：`%s`\n🎭 貼圖數量：%d",
		ratioDisplay, qualityDisplay, len(images))

	processingMsg, err := b.sendReplyToMessage(msg.ReplyToMessage, statusText)
	if err != nil {
		return
	}

	// 下載貼圖
	var downloadedImages []gemini.DownloadedImage
	for i, img := range images {
		b.updateMessageMarkdown(processingMsg, fmt.Sprintf("⏳ *處理中...*\n\n📏 比例：`%s`\n🎨 畫質：`%s`\n🎭 下載貼圖 %d/%d...",
			ratioDisplay, qualityDisplay, i+1, len(images)))

		fileConfig := tgbotapi.FileConfig{FileID: img.FileID}
		file, err := b.api.GetFile(fileConfig)
		if err != nil {
			b.updateMessageHTML(processingMsg, fmt.Sprintf("❌ <b>處理失敗</b>\n\n無法取得貼圖 %d\n\n<blockquote expandable>%s</blockquote>",
				i+1, truncateError(err.Error())))
			return
		}

		data, mimeType, err := b.downloadFile(file.FilePath)
		if err != nil {
			b.updateMessageHTML(processingMsg, fmt.Sprintf("❌ <b>處理失敗</b>\n\n下載貼圖 %d 失敗\n\n<blockquote expandable>%s</blockquote>",
				i+1, truncateError(err.Error())))
			return
		}

		downloadedImages = append(downloadedImages, gemini.DownloadedImage{
			Data:     data,
			MimeType: mimeType,
		})
	}

	// 比例規則：
	// 1. 使用者有指定 -> 使用指定值
	// 2. 有圖片但未指定 -> 使用最接近圖片比例的支援比例
	// 3. 沒圖片且未指定 -> 預設 1:1
	aspectRatio = resolveAspectRatio(params.AspectRatio, downloadedImages)
	ratioDisplay = ratioDisplayText(params.AspectRatio, aspectRatio, len(downloadedImages))

	b.updateMessageMarkdown(processingMsg, fmt.Sprintf("⏳ *同時生成中...*\n\n📏 比例：`%s`\n🎨 畫質：`%s`\n🎭 貼圖數量：%d",
		ratioDisplay, qualityDisplay, len(images)))

	// 收集圖片 FileID 供重試佇列使用
	var imageFileIDs []string
	for _, img := range images {
		imageFileIDs = append(imageFileIDs, img.FileID)
	}

	b.runAllGenerationTasks(msg, msg.ReplyToMessage.MessageID, prompt, quality, aspectRatio, downloadedImages, imageFileIDs, allServices, processingMsg.MessageID)
}

type imageData struct {
	FileID string
}

func (b *Bot) handlePhoto(msg *tgbotapi.Message) {
	// 解析參數
	caption := msg.Caption
	quality := ""
	withVoice := false
	customPrompt := ""

	// 檢查參數
	if strings.Contains(caption, "/s ") {
		// 解析畫質設定
		parts := strings.Split(caption, "/s ")
		if len(parts) > 1 {
			qParts := strings.Fields(parts[1])
			if len(qParts) > 0 {
				q := strings.ToUpper(qParts[0])
				if q == "1K" || q == "2K" || q == "4K" {
					quality = q
				}
			}
		}
		caption = strings.Split(caption, "/s")[0]
	}

	if strings.Contains(caption, "/v") {
		withVoice = true
		caption = strings.ReplaceAll(caption, "/v", "")
	}

	caption = strings.TrimSpace(caption)
	if caption != "" && !strings.HasPrefix(caption, "/") {
		customPrompt = caption
	}

	// 取得預設設定
	if quality == "" {
		quality, _ = b.db.GetUserSettings(msg.From.ID)
		if quality == "" {
			quality = "2K"
		}
	}

	// 決定使用的 Prompt
	prompt := config.DefaultPrompt
	if customPrompt != "" {
		prompt = customPrompt
		// 記錄到歷史
		b.db.AddToHistory(msg.From.ID, prompt)
	} else {
		// 檢查是否有使用者設定的預設
		defaultPrompt, _ := b.db.GetDefaultPrompt(msg.From.ID)
		if defaultPrompt != nil {
			prompt = defaultPrompt.Prompt
		}
	}

	allServices, _ := b.resolveAllServiceConfigs(msg.From.ID)
	if len(allServices) == 0 && !b.grokClient.Available() {
		reply := tgbotapi.NewMessage(msg.Chat.ID, "❌ 尚未設定任何服務，請先使用 /service add 新增服務或設定 GROK_API_KEY")
		reply.ReplyToMessageID = msg.MessageID
		b.api.Send(reply)
		return
	}

	serviceName := "多服務輪替"
	if len(allServices) == 1 {
		serviceName = allServices[0].Name
	}

	// 發送處理中訊息
	processingMsg, _ := b.api.Send(tgbotapi.NewMessage(msg.Chat.ID, "⏳ 處理中..."))

	// 下載圖片
	photo := msg.Photo[len(msg.Photo)-1] // 取最大的圖片
	fileConfig := tgbotapi.FileConfig{FileID: photo.FileID}
	file, err := b.api.GetFile(fileConfig)
	if err != nil {
		b.updateMessage(processingMsg, "❌ 無法取得圖片")
		return
	}

	imgData, mimeType, err := b.downloadFile(file.FilePath)
	if err != nil {
		b.updateMessage(processingMsg, "❌ 下載圖片失敗")
		return
	}

	// 取得圖片資訊並計算比例
	imageInfo, err := gemini.GetImageInfo(imgData)
	if err != nil {
		log.Printf("無法解析圖片資訊: %v", err)
		imageInfo = &gemini.ImageInfo{AspectRatio: defaultAspectRatio}
	}
	if imageInfo.AspectRatio == "" {
		imageInfo.AspectRatio = defaultAspectRatio
	}

	// 顯示圖片資訊
	ratioInfo := imageInfo.AspectRatio
	b.updateMessage(processingMsg, fmt.Sprintf("⏳ 處理中...\n🔌 服務: %s\n📐 圖片: %dx%d\n📏 比例: %s", serviceName, imageInfo.Width, imageInfo.Height, ratioInfo))

	// 重試邏輯：旋轉所有 Google 服務
	var result *gemini.ImageResult
	ctx := context.Background()
	var lastErr error
	googleSuccess := false

	for svcIdx, svcCfg := range allServices {
		gClient := gemini.NewClientWithService(svcCfg)
		for attempt := 0; attempt < 6; attempt++ {
			b.updateMessage(processingMsg, fmt.Sprintf("⏳ 處理中... (服務 %d/%d，嘗試 %d/6)\n🔌 服務: %s\n📐 圖片: %dx%d\n📏 比例: %s",
				svcIdx+1, len(allServices), attempt+1, svcCfg.Name, imageInfo.Width, imageInfo.Height, ratioInfo))

			result, lastErr = gClient.GenerateImage(ctx, imgData, mimeType, prompt, quality, imageInfo.AspectRatio)
			if lastErr == nil && result != nil && len(result.ImageData) > 0 {
				googleSuccess = true
				break
			}

			log.Printf("Google service %s attempt %d failed: %v", svcCfg.Name, attempt+1, lastErr)
			time.Sleep(time.Second * 2)
		}
		if googleSuccess {
			break
		}
	}

	// 如果 Google 服務全部失敗，嘗試 Grok
	if !googleSuccess && b.grokClient.Available() {
		b.updateMessage(processingMsg, "⏳ Google 服務失敗，嘗試 Grok 編輯...")

		for attempt := 0; attempt < 6; attempt++ {
			b.updateMessage(processingMsg, fmt.Sprintf("⏳ Grok 編輯中... (嘗試 %d/6)", attempt+1))

			grokResult, grokErr := b.grokClient.EditImage(ctx, imgData, prompt, "1024x1024")
			if grokErr == nil && grokResult != nil && len(grokResult.ImageData) > 0 {
				result = &gemini.ImageResult{ImageData: grokResult.ImageData}
				lastErr = nil
				break
			}
			lastErr = grokErr
			log.Printf("Grok edit attempt %d failed: %v", attempt+1, grokErr)
			time.Sleep(time.Second * 2)
		}
	}

	if result == nil || len(result.ImageData) == 0 {
		source := "google"
		if b.grokClient.Available() {
			source = "grok"
		}
		var svcCfg gemini.ServiceConfig
		if len(allServices) > 0 {
			svcCfg = allServices[0]
		}
		b.enqueueFailedGeneration(msg, msg.MessageID, failedGenerationPayload{
			Prompt:      prompt,
			Quality:     quality,
			AspectRatio: imageInfo.AspectRatio,
			ImageFileIDs: []string{
				photo.FileID,
			},
			Service: svcCfg,
		}, lastErr, source)

		errMsg := "unknown error"
		if lastErr != nil {
			errMsg = lastErr.Error()
		}
		b.updateMessage(processingMsg, fmt.Sprintf("❌ 處理失敗（所有服務均已嘗試）\n已加入失敗重試佇列，系統每 15 分鐘會隨機挑一筆再試一次。\n錯誤：%s", errMsg))
		return
	}

	// 如果需要語音
	var extractedText string
	var ttsResult *gemini.TTSResult

	if withVoice && len(allServices) > 0 {
		ttsClient := gemini.NewClientWithService(allServices[0])
		b.updateMessage(processingMsg, "⏳ 擷取文字中...")
		extractedText, _ = ttsClient.ExtractText(ctx, imgData, mimeType, config.ExtractTextPrompt)

		if extractedText != "" {
			b.updateMessage(processingMsg, "⏳ 生成語音中...")
			ttsResult, _ = ttsClient.GenerateTTS(ctx, extractedText, config.TTSVoiceName)
		}
	}

	// 刪除處理中訊息
	b.api.Request(tgbotapi.NewDeleteMessage(msg.Chat.ID, processingMsg.MessageID))

	// 發送結果
	if withVoice && ttsResult != nil {
		// 使用 Media Group 同時發送圖片和音訊
		mediaGroup := tgbotapi.NewMediaGroup(msg.Chat.ID, []interface{}{
			tgbotapi.NewInputMediaPhoto(tgbotapi.FileBytes{Name: "translated.png", Bytes: result.ImageData}),
			tgbotapi.NewInputMediaAudio(tgbotapi.FileBytes{Name: "voice.wav", Bytes: ttsResult.AudioData}),
		})
		mediaGroup.ReplyToMessageID = msg.MessageID
		b.api.SendMediaGroup(mediaGroup)
	} else {
		// 只發送圖片
		photoMsg := tgbotapi.NewPhoto(msg.Chat.ID, tgbotapi.FileBytes{Name: "translated.png", Bytes: result.ImageData})
		photoMsg.ReplyToMessageID = msg.MessageID
		b.api.Send(photoMsg)
	}
}

func (b *Bot) downloadFile(filePath string) ([]byte, string, error) {
	url := fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", b.config.BotToken, filePath)
	resp, err := http.Get(url)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}

	mimeType := "image/jpeg"
	if strings.HasSuffix(filePath, ".png") {
		mimeType = "image/png"
	}

	return data, mimeType, nil
}

func (b *Bot) updateMessage(msg tgbotapi.Message, text string) {
	edit := tgbotapi.NewEditMessageText(msg.Chat.ID, msg.MessageID, text)
	b.api.Send(edit)
}

func (b *Bot) updateMessageMarkdown(msg tgbotapi.Message, text string) {
	edit := tgbotapi.NewEditMessageText(msg.Chat.ID, msg.MessageID, text)
	edit.ParseMode = "Markdown"
	b.api.Send(edit)
}

func (b *Bot) updateMessageHTML(msg tgbotapi.Message, text string) {
	edit := tgbotapi.NewEditMessageText(msg.Chat.ID, msg.MessageID, text)
	edit.ParseMode = "HTML"
	b.api.Send(edit)
}

func (b *Bot) sendReplyMessage(msg *tgbotapi.Message, text string) (tgbotapi.Message, error) {
	reply := tgbotapi.NewMessage(msg.Chat.ID, text)
	reply.ParseMode = "Markdown"
	reply.ReplyToMessageID = msg.MessageID
	return b.api.Send(reply)
}

func (b *Bot) sendReplyToMessage(targetMsg *tgbotapi.Message, text string) (tgbotapi.Message, error) {
	reply := tgbotapi.NewMessage(targetMsg.Chat.ID, text)
	reply.ParseMode = "Markdown"
	reply.ReplyToMessageID = targetMsg.MessageID
	return b.api.Send(reply)
}

// tryGenerateVideo attempts to generate a video using Grok and upload it to TG.
func (b *Bot) tryGenerateVideo(chatID int64, replyToMessageID int, prompt string, imageURL string) {
	if !b.grokClient.Available() {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var videoResult *grok.VideoResult
	var lastErr error

	for attempt := 0; attempt < 6; attempt++ {
		videoResult, lastErr = b.grokClient.GenerateVideo(ctx, prompt, imageURL)
		if lastErr == nil && videoResult != nil && len(videoResult.VideoData) > 0 {
			break
		}
		log.Printf("Grok video attempt %d failed: %v", attempt+1, lastErr)
		time.Sleep(time.Second * 2)
	}

	if videoResult == nil || len(videoResult.VideoData) == 0 {
		log.Printf("Grok 影片生成失敗: %v", lastErr)
		return
	}

	// Upload video to TG
	videoMsg := tgbotapi.NewVideo(chatID, tgbotapi.FileBytes{Name: "generated.mp4", Bytes: videoResult.VideoData})
	videoMsg.ReplyToMessageID = replyToMessageID
	videoMsg.Caption = "🎬 AI 生成影片"
	if _, err := b.api.Send(videoMsg); err != nil {
		log.Printf("上傳影片失敗: %v", err)
	}
}
