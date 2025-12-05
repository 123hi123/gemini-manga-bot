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

	"gemini-manga-bot/config"
	"gemini-manga-bot/database"
	"gemini-manga-bot/gemini"

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
	db          *database.Database
	config      *config.Config
	mediaGroups *mediaGroupCache
}

func NewBot(cfg *config.Config, db *database.Database) (*Bot, error) {
	api, err := tgbotapi.NewBotAPI(cfg.BotToken)
	if err != nil {
		return nil, err
	}

	log.Printf("Bot authorized on account %s", api.Self.UserName)

	bot := &Bot{
		api:    api,
		gemini: gemini.NewClient(cfg.GeminiAPIKey),
		db:     db,
		config: cfg,
		mediaGroups: &mediaGroupCache{
			groups: make(map[string][]cachedImage),
		},
	}

	// 啟動清理過期快取的 goroutine
	go bot.cleanupMediaGroupCache()

	return bot, nil
}

func (b *Bot) Run() {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

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
	}
}

func (b *Bot) cmdStart(msg *tgbotapi.Message) {
	text := `🎨 *Gemini 漫畫翻譯 Bot*

歡迎使用！支援多種方式生成翻譯圖片。

*基本用法：*
• 直接輸入文字 → 使用 Prompt 生成圖片
• 回覆圖片/貼圖並輸入文字 → 將圖片＋文字一起處理
• 回覆文字並傳圖片/貼圖 → 將圖片＋文字一起處理

*群組使用：*
在群組中，文字訊息需以 ` + "`.`" + ` 開頭才會觸發
例如：` + "`.翻譯這張漫畫 @4K`" + `

*參數設定（用 @ 符號，前後需有空格）：*
• ` + "`@1:1`" + ` ` + "`@16:9`" + ` ` + "`@9:16`" + ` → 設定比例
• ` + "`@4K`" + ` ` + "`@2K`" + ` ` + "`@1K`" + ` → 設定畫質

*支援的比例：*
` + "`@1:1`" + ` ` + "`@2:3`" + ` ` + "`@3:2`" + ` ` + "`@3:4`" + ` ` + "`@4:3`" + ` ` + "`@4:5`" + ` ` + "`@5:4`" + ` ` + "`@9:16`" + ` ` + "`@16:9`" + ` ` + "`@21:9`" + `

*Auto 比例：* 不指定比例時，由 AI 自動決定

*範例：*
` + "`翻譯這張漫畫 @16:9 @4K`" + `

*指令：*
/save <名稱> <prompt> - 保存 Prompt
/list - 列出已保存的 Prompt
/history - 查看使用歷史
/setdefault - 設定預設 Prompt
/settings - 設定預設畫質
/delete - 刪除已保存的 Prompt
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
	Prompt       string
	AspectRatio  string // 如果沒指定則為空
	Quality      string // 如果沒指定則為空
	RatioError   string // 比例錯誤訊息
	QualityError string // 畫質錯誤訊息
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

	// 收集圖片
	var images []imageData

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

	// 不再自動偵測比例，完全讓 Gemini API 決定（除非使用者有指定）

	b.updateMessageMarkdown(processingMsg, fmt.Sprintf("⏳ *生成圖片中...*\n\n📏 比例：`%s`\n🎨 畫質：`%s`\n📸 圖片數量：%d",
		ratioDisplay, qualityDisplay, len(images)))

	// 重試邏輯：當前畫質三次 → 1K 三次
	var result *gemini.ImageResult
	qualities := []string{quality, quality, quality, "1K", "1K", "1K"}
	if quality == "1K" {
		qualities = []string{"1K", "1K", "1K", "1K", "1K", "1K"}
	}

	ctx := context.Background()
	var lastErr error

	for i, q := range qualities {
		b.updateMessageMarkdown(processingMsg, fmt.Sprintf("⏳ *生成圖片中...* (嘗試 %d/6，畫質 %s)\n\n📏 比例：`%s`\n🎨 畫質：`%s`\n📸 圖片數量：%d",
			i+1, q, ratioDisplay, qualityDisplay, len(images)))

		if len(downloadedImages) > 0 {
			// 有圖片的情況
			result, lastErr = b.gemini.GenerateImageWithContext(ctx, downloadedImages, prompt, q, aspectRatio)
		} else {
			// 純文字生成
			result, lastErr = b.gemini.GenerateImageFromText(ctx, prompt, q, aspectRatio)
		}

		if lastErr == nil {
			break
		}

		log.Printf("Attempt %d failed: %v", i+1, lastErr)
		time.Sleep(time.Second * 2)
	}

	if lastErr != nil {
		b.updateMessageHTML(processingMsg, fmt.Sprintf("❌ <b>處理失敗</b>（已重試 6 次）\n\n<blockquote expandable>%s</blockquote>",
			truncateError(lastErr.Error())))
		return
	}

	// 刪除處理中訊息
	b.api.Request(tgbotapi.NewDeleteMessage(msg.Chat.ID, processingMsg.MessageID))

	// 發送預覽圖（會被 Telegram 壓縮，方便快速查看）
	photoMsg := tgbotapi.NewPhoto(msg.Chat.ID, tgbotapi.FileBytes{Name: "preview.png", Bytes: result.ImageData})
	photoMsg.ReplyToMessageID = msg.MessageID
	b.api.Send(photoMsg)

	// 發送原檔案（不壓縮，完整畫質）
	docMsg := tgbotapi.NewDocument(msg.Chat.ID, tgbotapi.FileBytes{Name: fmt.Sprintf("generated_%s.png", quality), Bytes: result.ImageData})
	docMsg.ReplyToMessageID = msg.MessageID
	docMsg.Caption = "📎 原畫質檔案"
	b.api.Send(docMsg)
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

	// 收集圖片（從當前訊息）
	var images []imageData
	if len(msg.Photo) > 0 {
		// 檢查是否屬於 Media Group
		if msg.MediaGroupID != "" {
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

	// 不再自動偵測比例，完全讓 Gemini API 決定（除非使用者有指定）

	b.updateMessageMarkdown(processingMsg, fmt.Sprintf("⏳ *生成圖片中...*\n\n📏 比例：`%s`\n🎨 畫質：`%s`\n📸 圖片數量：%d",
		ratioDisplay, qualityDisplay, len(images)))

	// 重試邏輯
	var result *gemini.ImageResult
	qualities := []string{quality, quality, quality, "1K", "1K", "1K"}
	if quality == "1K" {
		qualities = []string{"1K", "1K", "1K", "1K", "1K", "1K"}
	}

	ctx := context.Background()
	var lastErr error

	for i, q := range qualities {
		b.updateMessageMarkdown(processingMsg, fmt.Sprintf("⏳ *生成圖片中...* (嘗試 %d/6，畫質 %s)\n\n📏 比例：`%s`\n🎨 畫質：`%s`\n📸 圖片數量：%d",
			i+1, q, ratioDisplay, qualityDisplay, len(images)))

		result, lastErr = b.gemini.GenerateImageWithContext(ctx, downloadedImages, prompt, q, aspectRatio)
		if lastErr == nil {
			break
		}

		log.Printf("Attempt %d failed: %v", i+1, lastErr)
		time.Sleep(time.Second * 2)
	}

	if lastErr != nil {
		b.updateMessageHTML(processingMsg, fmt.Sprintf("❌ <b>處理失敗</b>（已重試 6 次）\n\n<blockquote expandable>%s</blockquote>",
			truncateError(lastErr.Error())))
		return
	}

	// 刪除處理中訊息
	b.api.Request(tgbotapi.NewDeleteMessage(msg.Chat.ID, processingMsg.MessageID))

	// 發送預覽圖（會被 Telegram 壓縮，方便快速查看）
	photoMsg := tgbotapi.NewPhoto(msg.Chat.ID, tgbotapi.FileBytes{Name: "preview.png", Bytes: result.ImageData})
	photoMsg.ReplyToMessageID = msg.ReplyToMessage.MessageID
	b.api.Send(photoMsg)

	// 發送原檔案（不壓縮，完整畫質）
	docMsg := tgbotapi.NewDocument(msg.Chat.ID, tgbotapi.FileBytes{Name: fmt.Sprintf("generated_%s.png", quality), Bytes: result.ImageData})
	docMsg.ReplyToMessageID = msg.ReplyToMessage.MessageID
	docMsg.Caption = "📎 原畫質檔案"
	b.api.Send(docMsg)
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

	// 不再自動偵測比例，完全讓 Gemini API 決定（除非使用者有指定）

	b.updateMessageMarkdown(processingMsg, fmt.Sprintf("⏳ *生成圖片中...*\n\n📏 比例：`%s`\n🎨 畫質：`%s`\n🎭 貼圖數量：%d",
		ratioDisplay, qualityDisplay, len(images)))

	// 重試邏輯
	var result *gemini.ImageResult
	qualities := []string{quality, quality, quality, "1K", "1K", "1K"}
	if quality == "1K" {
		qualities = []string{"1K", "1K", "1K", "1K", "1K", "1K"}
	}

	ctx := context.Background()
	var lastErr error

	for i, q := range qualities {
		b.updateMessageMarkdown(processingMsg, fmt.Sprintf("⏳ *生成圖片中...* (嘗試 %d/6，畫質 %s)\n\n📏 比例：`%s`\n🎨 畫質：`%s`\n🎭 貼圖數量：%d",
			i+1, q, ratioDisplay, qualityDisplay, len(images)))

		result, lastErr = b.gemini.GenerateImageWithContext(ctx, downloadedImages, prompt, q, aspectRatio)
		if lastErr == nil {
			break
		}

		log.Printf("Attempt %d failed: %v", i+1, lastErr)
		time.Sleep(time.Second * 2)
	}

	if lastErr != nil {
		b.updateMessageHTML(processingMsg, fmt.Sprintf("❌ <b>處理失敗</b>（已重試 6 次）\n\n<blockquote expandable>%s</blockquote>",
			truncateError(lastErr.Error())))
		return
	}

	// 刪除處理中訊息
	b.api.Request(tgbotapi.NewDeleteMessage(msg.Chat.ID, processingMsg.MessageID))

	// 發送預覽圖（會被 Telegram 壓縮，方便快速查看）
	photoMsg := tgbotapi.NewPhoto(msg.Chat.ID, tgbotapi.FileBytes{Name: "preview.png", Bytes: result.ImageData})
	photoMsg.ReplyToMessageID = msg.ReplyToMessage.MessageID
	b.api.Send(photoMsg)

	// 發送原檔案（不壓縮，完整畫質）
	docMsg := tgbotapi.NewDocument(msg.Chat.ID, tgbotapi.FileBytes{Name: fmt.Sprintf("generated_%s.png", quality), Bytes: result.ImageData})
	docMsg.ReplyToMessageID = msg.ReplyToMessage.MessageID
	docMsg.Caption = "📎 原畫質檔案"
	b.api.Send(docMsg)
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

	imageData, mimeType, err := b.downloadFile(file.FilePath)
	if err != nil {
		b.updateMessage(processingMsg, "❌ 下載圖片失敗")
		return
	}

	// 取得圖片資訊並計算比例
	imageInfo, err := gemini.GetImageInfo(imageData)
	if err != nil {
		log.Printf("無法解析圖片資訊: %v", err)
		imageInfo = &gemini.ImageInfo{AspectRatio: ""} // 讓模型自動決定
	}

	// 顯示圖片資訊
	ratioInfo := "自動"
	if imageInfo.AspectRatio != "" {
		ratioInfo = imageInfo.AspectRatio
	}
	b.updateMessage(processingMsg, fmt.Sprintf("⏳ 處理中...\n📐 圖片: %dx%d\n📏 比例: %s", imageInfo.Width, imageInfo.Height, ratioInfo))

	// 重試邏輯：2K 三次 → 1K 三次
	var result *gemini.ImageResult
	qualities := []string{quality, quality, quality, "1K", "1K", "1K"}
	if quality == "1K" {
		qualities = []string{"1K", "1K", "1K", "1K", "1K", "1K"}
	}

	ctx := context.Background()
	var lastErr error

	for i, q := range qualities {
		b.updateMessage(processingMsg, fmt.Sprintf("⏳ 處理中... (嘗試 %d/6，畫質 %s)\n📐 圖片: %dx%d\n📏 比例: %s", i+1, q, imageInfo.Width, imageInfo.Height, ratioInfo))

		result, lastErr = b.gemini.GenerateImage(ctx, imageData, mimeType, prompt, q, imageInfo.AspectRatio)
		if lastErr == nil {
			break
		}

		log.Printf("Attempt %d failed: %v", i+1, lastErr)
		time.Sleep(time.Second * 2)
	}

	if lastErr != nil {
		b.updateMessage(processingMsg, fmt.Sprintf("❌ 處理失敗（已重試 6 次）\n錯誤：%s", lastErr.Error()))
		return
	}

	// 如果需要語音
	var extractedText string
	var ttsResult *gemini.TTSResult

	if withVoice {
		b.updateMessage(processingMsg, "⏳ 擷取文字中...")
		extractedText, _ = b.gemini.ExtractText(ctx, imageData, mimeType, config.ExtractTextPrompt)

		if extractedText != "" {
			b.updateMessage(processingMsg, "⏳ 生成語音中...")
			ttsResult, _ = b.gemini.GenerateTTS(ctx, extractedText, config.TTSVoiceName)
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
