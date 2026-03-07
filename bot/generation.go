package bot

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"tg-bawer/gemini"
	"tg-bawer/grok"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const (
	taskTypeGoogleImage = "google_image"
	taskTypeGrokImage   = "grok_image"
	taskTypeGrokVideo   = "grok_video"
	maxRetryCount       = 100
)

// runAllGenerationTasks launches Google image, Grok image, and Grok video generation
// tasks concurrently. Each task sends its result (or queues a failure) independently.
// The status message is deleted or updated after all tasks complete.
func (b *Bot) runAllGenerationTasks(
	msg *tgbotapi.Message,
	replyToMsgID int,
	prompt, quality, aspectRatio string,
	downloadedImages []gemini.DownloadedImage,
	imageFileIDs []string,
	allServices []gemini.ServiceConfig,
	statusMsgID int,
) {
	var wg sync.WaitGroup

	// resultCh collects success/failure for image tasks (Google + Grok image).
	// Grok video is treated as optional/best-effort and does not report into this
	// channel; its failures are silently enqueued for retry via /queue.
	resultCh := make(chan bool, 2)

	// Google image task
	if len(allServices) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resultCh <- b.runGoogleImageTask(msg, replyToMsgID, prompt, quality, aspectRatio, downloadedImages, imageFileIDs, allServices)
		}()
	}

	// Grok image and video tasks
	if b.grokClient.Available() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resultCh <- b.runGrokImageTask(msg, replyToMsgID, prompt, downloadedImages, imageFileIDs)
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			b.runGrokVideoTask(msg, replyToMsgID, prompt)
		}()
	}

	go func() {
		wg.Wait()
		close(resultCh)

		failedCount := 0
		for ok := range resultCh {
			if !ok {
				failedCount++
			}
		}

		if statusMsgID > 0 {
			if failedCount == 0 {
				b.api.Request(tgbotapi.NewDeleteMessage(msg.Chat.ID, statusMsgID))
			} else {
				edit := tgbotapi.NewEditMessageText(msg.Chat.ID, statusMsgID,
					fmt.Sprintf("⚠️ %d 個子任務失敗，已加入重試佇列。\n用 /queue 查看詳情。", failedCount))
				b.api.Send(edit)
			}
		}
	}()
}

// runGoogleImageTask runs Google image generation with all available services.
// Returns true on success, false on failure (failure is enqueued for retry).
func (b *Bot) runGoogleImageTask(
	msg *tgbotapi.Message,
	replyToMsgID int,
	prompt, quality, aspectRatio string,
	downloadedImages []gemini.DownloadedImage,
	imageFileIDs []string,
	allServices []gemini.ServiceConfig,
) bool {
	ctx := context.Background()
	var result *gemini.ImageResult
	var lastErr error

	for _, svcCfg := range allServices {
		gClient := gemini.NewClientWithService(svcCfg)
		for attempt := 0; attempt < 6; attempt++ {
			if len(downloadedImages) > 0 {
				result, lastErr = gClient.GenerateImageWithContext(ctx, downloadedImages, prompt, quality, aspectRatio)
			} else {
				result, lastErr = gClient.GenerateImageFromText(ctx, prompt, quality, aspectRatio)
			}
			if lastErr == nil && result != nil && len(result.ImageData) > 0 {
				break
			}
			log.Printf("Google service %s attempt %d failed: %v", svcCfg.Name, attempt+1, lastErr)
			time.Sleep(2 * time.Second)
		}
		if result != nil && len(result.ImageData) > 0 {
			break
		}
	}

	if result == nil || len(result.ImageData) == 0 {
		var svcCfg gemini.ServiceConfig
		if len(allServices) > 0 {
			svcCfg = allServices[0]
		}
		b.enqueueFailedGeneration(msg, replyToMsgID, failedGenerationPayload{
			Prompt:       prompt,
			Quality:      quality,
			AspectRatio:  aspectRatio,
			ImageFileIDs: imageFileIDs,
			Service:      svcCfg,
		}, lastErr, taskTypeGoogleImage)
		log.Printf("Google 圖片生成失敗，已加入重試佇列")
		return false
	}

	// Send compressed preview
	photoMsg := tgbotapi.NewPhoto(msg.Chat.ID, tgbotapi.FileBytes{Name: "google_preview.png", Bytes: result.ImageData})
	photoMsg.ReplyToMessageID = replyToMsgID
	photoMsg.Caption = "🌐 Google 圖片"
	if _, err := b.api.Send(photoMsg); err != nil {
		log.Printf("發送 Google 預覽圖失敗: %v", err)
	}

	// Send full-quality document for 2K/4K
	if quality == "4K" || quality == "2K" {
		docMsg := tgbotapi.NewDocument(msg.Chat.ID, tgbotapi.FileBytes{
			Name:  fmt.Sprintf("google_generated_%s.png", quality),
			Bytes: result.ImageData,
		})
		docMsg.ReplyToMessageID = replyToMsgID
		docMsg.Caption = "📎 Google 原畫質"
		if _, err := b.api.Send(docMsg); err != nil {
			log.Printf("發送 Google 原檔案失敗: %v", err)
		}
	}

	return true
}

// runGrokImageTask runs Grok image generation (or editing if images are provided).
// Returns true on success, false on failure (failure is enqueued for retry).
func (b *Bot) runGrokImageTask(
	msg *tgbotapi.Message,
	replyToMsgID int,
	prompt string,
	downloadedImages []gemini.DownloadedImage,
	imageFileIDs []string,
) bool {
	ctx := context.Background()
	var result *grok.ImageResult
	var lastErr error

	for attempt := 0; attempt < 6; attempt++ {
		if len(downloadedImages) > 0 {
			result, lastErr = b.grokClient.EditImage(ctx, downloadedImages[0].Data, prompt, "1024x1024")
		} else {
			result, lastErr = b.grokClient.GenerateImage(ctx, prompt, "1024x1024")
		}
		if lastErr == nil && result != nil && len(result.ImageData) > 0 {
			break
		}
		log.Printf("Grok image attempt %d failed: %v", attempt+1, lastErr)
		time.Sleep(2 * time.Second)
	}

	if result == nil || len(result.ImageData) == 0 {
		b.enqueueFailedGeneration(msg, replyToMsgID, failedGenerationPayload{
			Prompt:       prompt,
			ImageFileIDs: imageFileIDs,
		}, lastErr, taskTypeGrokImage)
		log.Printf("Grok 圖片生成失敗，已加入重試佇列")
		return false
	}

	photoMsg := tgbotapi.NewPhoto(msg.Chat.ID, tgbotapi.FileBytes{Name: "grok_preview.png", Bytes: result.ImageData})
	photoMsg.ReplyToMessageID = replyToMsgID
	photoMsg.Caption = "🤖 Grok 圖片"
	if _, err := b.api.Send(photoMsg); err != nil {
		log.Printf("發送 Grok 預覽圖失敗: %v", err)
	}

	return true
}

// runGrokVideoTask runs Grok video generation and uploads the result.
// Failures are silently enqueued for retry (no error message sent to user).
func (b *Bot) runGrokVideoTask(
	msg *tgbotapi.Message,
	replyToMsgID int,
	prompt string,
) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var result *grok.VideoResult
	var lastErr error

	for attempt := 0; attempt < 6; attempt++ {
		result, lastErr = b.grokClient.GenerateVideo(ctx, prompt, "")
		if lastErr == nil && result != nil && len(result.VideoData) > 0 {
			break
		}
		log.Printf("Grok video attempt %d failed: %v", attempt+1, lastErr)
		time.Sleep(2 * time.Second)
	}

	if result == nil || len(result.VideoData) == 0 {
		b.enqueueFailedGeneration(msg, replyToMsgID, failedGenerationPayload{
			Prompt: prompt,
		}, lastErr, taskTypeGrokVideo)
		log.Printf("Grok 影片生成失敗，已加入重試佇列")
		return
	}

	videoMsg := tgbotapi.NewVideo(msg.Chat.ID, tgbotapi.FileBytes{Name: "generated.mp4", Bytes: result.VideoData})
	videoMsg.ReplyToMessageID = replyToMsgID
	videoMsg.Caption = "🎬 Grok 影片"
	if _, err := b.api.Send(videoMsg); err != nil {
		log.Printf("上傳影片失敗: %v", err)
	}
}
