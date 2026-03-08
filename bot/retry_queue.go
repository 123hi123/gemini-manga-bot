package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"tg-bawer/database"
	"tg-bawer/gemini"
	"tg-bawer/grok"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type failedGenerationPayload struct {
	Prompt       string               `json:"prompt"`
	Quality      string               `json:"quality"`
	AspectRatio  string               `json:"aspect_ratio,omitempty"`
	ImageFileIDs []string             `json:"image_file_ids,omitempty"`
	Service      gemini.ServiceConfig `json:"service"`
}

func buildRetryQualities(quality string) []string {
	if quality == "" {
		quality = "2K"
	}
	return []string{quality, quality, quality, quality, quality, quality}
}

func (b *Bot) enqueueFailedGeneration(msg *tgbotapi.Message, replyToMessageID int, payload failedGenerationPayload, lastErr error, source string) {
	if msg == nil || msg.From == nil {
		return
	}

	rawPayload, err := json.Marshal(payload)
	if err != nil {
		log.Printf("序列化失敗任務失敗: %v", err)
		return
	}

	lastError := ""
	if lastErr != nil {
		lastError = truncateError(lastErr.Error())
	}

	if source == "" {
		source = "google"
	}

	if err := b.db.AddFailedGeneration(msg.From.ID, msg.Chat.ID, int64(replyToMessageID), string(rawPayload), lastError, source); err != nil {
		log.Printf("寫入失敗任務失敗: %v", err)
	}
}

func (b *Bot) retryFailedGenerations() {
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		b.retryOneFailedGeneration()
	}
}

func (b *Bot) retryOneFailedGeneration() {
	task, err := b.db.GetRandomFailedGeneration()
	if err != nil {
		log.Printf("讀取失敗任務失敗: %v", err)
		return
	}
	if task == nil {
		return
	}

	// Delete tasks that have exceeded the maximum retry count
	if task.RetryCount >= maxRetryCount {
		log.Printf("任務達到最大重試次數 %d (id=%d)，刪除", maxRetryCount, task.ID)
		// Decrement image ref counts for file IDs stored in this task's payload
		var expiredPayload failedGenerationPayload
		if jsonErr := json.Unmarshal([]byte(task.Payload), &expiredPayload); jsonErr == nil {
			for _, fileID := range expiredPayload.ImageFileIDs {
				if err := b.db.DecrementImageRefCountByFileID(task.UserID, task.ChatID, fileID); err != nil {
					log.Printf("警告：減少圖片引用計數失敗，可能導致資料庫殘留孤立記錄 (id=%d, file=%s): %v", task.ID, fileID, err)
				}
			}
		}
		b.db.DeleteFailedGeneration(task.ID)
		return
	}

	var payload failedGenerationPayload
	if err := json.Unmarshal([]byte(task.Payload), &payload); err != nil {
		log.Printf("解析失敗任務 payload 失敗 (id=%d): %v", task.ID, err)
		b.db.DeleteFailedGeneration(task.ID)
		return
	}

	// Normalise legacy source values to the current task type constants
	source := task.Source
	switch source {
	case "", "google":
		source = taskTypeGoogleImage
	case "grok":
		source = taskTypeGrokImage
	}

	// Handle Grok video retry separately
	if source == taskTypeGrokVideo {
		b.retryGrokVideoTask(task, payload)
		return
	}

	downloadedImages, err := b.downloadImagesByFileIDs(payload.ImageFileIDs)
	if err != nil {
		b.db.MarkFailedGenerationRetry(task.ID, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	aspectRatio := resolveAspectRatio(payload.AspectRatio, downloadedImages)

	var result *gemini.ImageResult
	resultSource := source // track which provider actually succeeded

	grokClient := b.resolveGrokClient(task.UserID)
	if source == taskTypeGrokImage && grokClient != nil {
		// Grok image retry
		gc := grokClient
		for attempt := 0; attempt < 6; attempt++ {
			var grokResult *grok.ImageResult
			if len(downloadedImages) > 0 {
				grokResult, err = gc.EditImage(ctx, downloadedImages[0].Data, payload.Prompt, "1024x1024")
			} else {
				grokResult, err = gc.GenerateImage(ctx, payload.Prompt, "1024x1024")
			}
			if err == nil && grokResult != nil && len(grokResult.ImageData) > 0 {
				result = &gemini.ImageResult{ImageData: grokResult.ImageData}
				break
			}
			log.Printf("Grok retry attempt %d failed (id=%d): %v", attempt+1, task.ID, err)
			b.addErrorLog("Grok 圖片重試",
				fmt.Sprintf("task_id=%d, attempt=%d, prompt=%q, size=1024x1024, images=%d", task.ID, attempt+1, payload.Prompt, len(downloadedImages)),
				fmt.Sprintf("%v", err))
			time.Sleep(time.Second * 2)
		}
	} else {
		// Google image retry: try all user services
		allServices, _ := b.resolveAllServiceConfigs(task.UserID)
		if len(allServices) == 0 {
			// Fallback to payload service
			if payload.Service.APIKey != "" {
				allServices = append(allServices, payload.Service)
			}
		}

		for _, svcCfg := range allServices {
			client := gemini.NewClientWithService(svcCfg)
			for attempt := 0; attempt < 6; attempt++ {
				if len(downloadedImages) > 0 {
					result, err = client.GenerateImageWithContext(ctx, downloadedImages, payload.Prompt, payload.Quality, aspectRatio)
				} else {
					result, err = client.GenerateImageFromText(ctx, payload.Prompt, payload.Quality, aspectRatio)
				}
				if err == nil && result != nil && len(result.ImageData) > 0 {
					break
				}
				log.Printf("Google retry service %s attempt %d failed (id=%d): %v", svcCfg.Name, attempt+1, task.ID, err)
				b.addErrorLog("Google 圖片重試",
					fmt.Sprintf("task_id=%d, service=%s, attempt=%d, prompt=%q, quality=%s, aspect_ratio=%s, images=%d", task.ID, svcCfg.Name, attempt+1, payload.Prompt, payload.Quality, aspectRatio, len(downloadedImages)),
					fmt.Sprintf("%v", err))
				time.Sleep(time.Second * 2)
			}
			if result != nil && len(result.ImageData) > 0 {
				resultSource = taskTypeGoogleImage
				break
			}
		}
	}

	if result == nil || len(result.ImageData) == 0 {
		errMsg := "unknown error"
		if err != nil {
			errMsg = err.Error()
		}
		b.db.MarkFailedGenerationRetry(task.ID, errMsg)
		log.Printf("定時重試失敗 (id=%d): %v", task.ID, err)
		b.addErrorLog("圖片重試最終失敗",
			fmt.Sprintf("task_id=%d, prompt=%q, quality=%s", task.ID, payload.Prompt, payload.Quality),
			fmt.Sprintf("%v", err))
		return
	}

	if err := b.sendRetrySuccessResult(task, payload, result, resultSource); err != nil {
		b.db.MarkFailedGenerationRetry(task.ID, err.Error())
		log.Printf("定時重試成功但發送失敗 (id=%d): %v", task.ID, err)
		return
	}

	if err := b.db.DeleteFailedGeneration(task.ID); err != nil {
		log.Printf("刪除已成功重試任務失敗 (id=%d): %v", task.ID, err)
	}
}

// retryGrokVideoTask handles retry for a grok_video type failed task.
func (b *Bot) retryGrokVideoTask(task *database.FailedGeneration, payload failedGenerationPayload) {
	gc := b.resolveGrokClient(task.UserID)
	if gc == nil {
		b.db.MarkFailedGenerationRetry(task.ID, "Grok not available")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var videoResult *grok.VideoResult
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		videoResult, lastErr = gc.GenerateVideo(ctx, payload.Prompt, "")
		if lastErr == nil && videoResult != nil && len(videoResult.VideoData) > 0 {
			break
		}
		log.Printf("Grok video retry attempt %d failed (id=%d): %v", attempt+1, task.ID, lastErr)
		b.addErrorLog("Grok 影片重試",
			fmt.Sprintf("task_id=%d, attempt=%d, prompt=%q", task.ID, attempt+1, payload.Prompt),
			fmt.Sprintf("%v", lastErr))
		time.Sleep(time.Second * 2)
	}

	if videoResult == nil || len(videoResult.VideoData) == 0 {
		errMsg := "unknown error"
		if lastErr != nil {
			errMsg = lastErr.Error()
		}
		b.db.MarkFailedGenerationRetry(task.ID, errMsg)
		log.Printf("定時重試影片失敗 (id=%d): %v", task.ID, lastErr)
		b.addErrorLog("Grok 影片重試最終失敗",
			fmt.Sprintf("task_id=%d, prompt=%q", task.ID, payload.Prompt),
			fmt.Sprintf("%v", lastErr))
		return
	}

	notice := tgbotapi.NewMessage(task.ChatID, fmt.Sprintf("♻️ 自動重試成功（影片任務 #%d）", task.ID))
	if task.ReplyToMessageID > 0 {
		notice.ReplyToMessageID = int(task.ReplyToMessageID)
	}
	b.api.Send(notice)

	videoMsg := tgbotapi.NewVideo(task.ChatID, tgbotapi.FileBytes{Name: "retry_generated.mp4", Bytes: videoResult.VideoData})
	if task.ReplyToMessageID > 0 {
		videoMsg.ReplyToMessageID = int(task.ReplyToMessageID)
	}
	videoMsg.Caption = "🎬 定時重試影片"
	b.api.Send(videoMsg)

	if err := b.db.DeleteFailedGeneration(task.ID); err != nil {
		log.Printf("刪除已成功重試影片任務失敗 (id=%d): %v", task.ID, err)
	}
}

func (b *Bot) downloadImagesByFileIDs(fileIDs []string) ([]gemini.DownloadedImage, error) {
	if len(fileIDs) == 0 {
		return nil, nil
	}

	downloadedImages := make([]gemini.DownloadedImage, 0, len(fileIDs))
	for _, fileID := range fileIDs {
		file, err := b.api.GetFile(tgbotapi.FileConfig{FileID: fileID})
		if err != nil {
			return nil, err
		}

		data, mimeType, err := b.downloadFile(file.FilePath)
		if err != nil {
			return nil, err
		}

		downloadedImages = append(downloadedImages, gemini.DownloadedImage{
			Data:     data,
			MimeType: mimeType,
		})
	}

	return downloadedImages, nil
}

func (b *Bot) sendRetrySuccessResult(task *database.FailedGeneration, payload failedGenerationPayload, result *gemini.ImageResult, resultSource string) error {
	if result == nil || len(result.ImageData) == 0 {
		return fmt.Errorf("empty retry result")
	}

	// Build a human-readable label for the result source
	var sourceLabel string
	switch resultSource {
	case taskTypeGrokImage:
		sourceLabel = "🤖 Grok 圖片"
	case taskTypeGoogleImage:
		sourceLabel = "🌐 Google 圖片"
	default:
		sourceLabel = "🖼 圖片"
		log.Printf("sendRetrySuccessResult: unexpected resultSource=%q (task #%d)", resultSource, task.ID)
	}

	notice := tgbotapi.NewMessage(task.ChatID, fmt.Sprintf("♻️ 自動重試成功（任務 #%d）\n結果來源：%s", task.ID, sourceLabel))
	if task.ReplyToMessageID > 0 {
		notice.ReplyToMessageID = int(task.ReplyToMessageID)
	}
	if _, err := b.api.Send(notice); err != nil {
		return err
	}

	photoMsg := tgbotapi.NewPhoto(task.ChatID, tgbotapi.FileBytes{Name: "retry_preview.png", Bytes: result.ImageData})
	if task.ReplyToMessageID > 0 {
		photoMsg.ReplyToMessageID = int(task.ReplyToMessageID)
	}
	photoMsg.Caption = sourceLabel
	sentPhoto, err := b.api.Send(photoMsg)
	if err != nil {
		return fmt.Errorf("發送預覽圖失敗: %w", err)
	}
	// Verify the photo was actually uploaded
	if len(sentPhoto.Photo) == 0 {
		return fmt.Errorf("預覽圖上傳失敗：未收到確認")
	}

	filename := "retry_generated.png"
	if payload.Quality != "" {
		filename = fmt.Sprintf("retry_generated_%s.png", payload.Quality)
	}
	docMsg := tgbotapi.NewDocument(task.ChatID, tgbotapi.FileBytes{Name: filename, Bytes: result.ImageData})
	docMsg.Caption = fmt.Sprintf("📎 定時重試輸出（原畫質）｜%s", sourceLabel)
	if task.ReplyToMessageID > 0 {
		docMsg.ReplyToMessageID = int(task.ReplyToMessageID)
	}
	sentDoc, err := b.api.Send(docMsg)
	if err != nil {
		return fmt.Errorf("發送原檔案失敗: %w", err)
	}
	// Verify the document was actually uploaded
	if sentDoc.Document == nil {
		return fmt.Errorf("原檔案上傳失敗：未收到確認")
	}

	return nil
}
