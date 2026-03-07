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

	if source == taskTypeGrokImage && b.grokClient.Available() {
		// Grok image retry
		for attempt := 0; attempt < 6; attempt++ {
			var grokResult *grok.ImageResult
			if len(downloadedImages) > 0 {
				grokResult, err = b.grokClient.EditImage(ctx, downloadedImages[0].Data, payload.Prompt, "1024x1024")
			} else {
				grokResult, err = b.grokClient.GenerateImage(ctx, payload.Prompt, "1024x1024")
			}
			if err == nil && grokResult != nil && len(grokResult.ImageData) > 0 {
				result = &gemini.ImageResult{ImageData: grokResult.ImageData}
				break
			}
			log.Printf("Grok retry attempt %d failed (id=%d): %v", attempt+1, task.ID, err)
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
				time.Sleep(time.Second * 2)
			}
			if result != nil && len(result.ImageData) > 0 {
				break
			}
		}

		// If Google fails, try Grok as fallback
		if (result == nil || len(result.ImageData) == 0) && b.grokClient.Available() {
			for attempt := 0; attempt < 6; attempt++ {
				var grokResult *grok.ImageResult
				if len(downloadedImages) > 0 {
					grokResult, err = b.grokClient.EditImage(ctx, downloadedImages[0].Data, payload.Prompt, "1024x1024")
				} else {
					grokResult, err = b.grokClient.GenerateImage(ctx, payload.Prompt, "1024x1024")
				}
				if err == nil && grokResult != nil && len(grokResult.ImageData) > 0 {
					result = &gemini.ImageResult{ImageData: grokResult.ImageData}
					break
				}
				log.Printf("Grok retry fallback attempt %d failed (id=%d): %v", attempt+1, task.ID, err)
				time.Sleep(time.Second * 2)
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
		return
	}

	if err := b.sendRetrySuccessResult(task, payload, result); err != nil {
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
	if !b.grokClient.Available() {
		b.db.MarkFailedGenerationRetry(task.ID, "Grok not available")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var videoResult *grok.VideoResult
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		videoResult, lastErr = b.grokClient.GenerateVideo(ctx, payload.Prompt, "")
		if lastErr == nil && videoResult != nil && len(videoResult.VideoData) > 0 {
			break
		}
		log.Printf("Grok video retry attempt %d failed (id=%d): %v", attempt+1, task.ID, lastErr)
		time.Sleep(time.Second * 2)
	}

	if videoResult == nil || len(videoResult.VideoData) == 0 {
		errMsg := "unknown error"
		if lastErr != nil {
			errMsg = lastErr.Error()
		}
		b.db.MarkFailedGenerationRetry(task.ID, errMsg)
		log.Printf("定時重試影片失敗 (id=%d): %v", task.ID, lastErr)
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

func (b *Bot) sendRetrySuccessResult(task *database.FailedGeneration, payload failedGenerationPayload, result *gemini.ImageResult) error {
	if result == nil || len(result.ImageData) == 0 {
		return fmt.Errorf("empty retry result")
	}

	notice := tgbotapi.NewMessage(task.ChatID, fmt.Sprintf("♻️ 自動重試成功（任務 #%d）", task.ID))
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
	docMsg.Caption = "📎 定時重試輸出（原畫質）"
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
