package grok

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// Client is the Grok API client for image generation, image editing, and video generation.
type Client struct {
	apiKey     string
	baseURL    string
	imgModel   string
	editModel  string
	videoModel string
	httpClient *http.Client
}

// ImageResult holds the generated image data.
type ImageResult struct {
	ImageData []byte
}

// VideoResult holds the generated video data.
type VideoResult struct {
	VideoData []byte
}

const (
	ServiceTypeGrok = "grok"

	DefaultBaseURL    = "http://127.0.0.1:8000"
	DefaultImgModel   = "grok-imagine-1.0"
	DefaultEditModel  = "grok-imagine-1.0-edit"
	DefaultVideoModel = "grok-imagine-1.0-video"
)

// NewClient creates a new Grok client.
func NewClient(apiKey, baseURL, imgModel, editModel, videoModel string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if imgModel == "" {
		imgModel = DefaultImgModel
	}
	if editModel == "" {
		editModel = DefaultEditModel
	}
	if videoModel == "" {
		videoModel = DefaultVideoModel
	}
	return &Client{
		apiKey:     apiKey,
		baseURL:    strings.TrimRight(baseURL, "/"),
		imgModel:   imgModel,
		editModel:  editModel,
		videoModel: videoModel,
		httpClient: &http.Client{
			Timeout: 180 * time.Second,
		},
	}
}

// Available returns true if the Grok client has been configured with an API key.
func (c *Client) Available() bool {
	return strings.TrimSpace(c.apiKey) != ""
}

// GenerateImage generates an image from a text prompt.
func (c *Client) GenerateImage(ctx context.Context, prompt, size string) (*ImageResult, error) {
	if size == "" {
		size = "1024x1024"
	}

	requestBody := map[string]interface{}{
		"model": c.imgModel,
		"messages": []map[string]interface{}{
			{
				"role":    "user",
				"content": prompt,
			},
		},
		"image_config": map[string]interface{}{
			"n":               1,
			"size":            size,
			"response_format": "url",
		},
		"stream": false,
	}

	jsonBody, err := json.Marshal(requestBody)
	if err != nil {
		return nil, fmt.Errorf("grok: marshal request: %w", err)
	}

	endpoint := c.baseURL + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewBuffer(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("grok: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("grok: send request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("grok: read response: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("grok: API error (status %d): %s", resp.StatusCode, string(body))
	}

	imageURL, err := extractImageURL(body)
	if err != nil {
		return nil, err
	}

	imageData, err := downloadURL(ctx, imageURL)
	if err != nil {
		return nil, fmt.Errorf("grok: download image: %w", err)
	}

	if len(imageData) == 0 {
		return nil, fmt.Errorf("grok: downloaded image is empty")
	}

	return &ImageResult{ImageData: imageData}, nil
}

// EditImage edits an image with a text prompt.
func (c *Client) EditImage(ctx context.Context, imageData []byte, prompt, size string) (*ImageResult, error) {
	if size == "" {
		size = "1024x1024"
	}

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	if err := writer.WriteField("model", c.editModel); err != nil {
		return nil, fmt.Errorf("grok: write model field: %w", err)
	}
	if err := writer.WriteField("prompt", prompt); err != nil {
		return nil, fmt.Errorf("grok: write prompt field: %w", err)
	}
	if err := writer.WriteField("n", "1"); err != nil {
		return nil, fmt.Errorf("grok: write n field: %w", err)
	}
	if err := writer.WriteField("size", size); err != nil {
		return nil, fmt.Errorf("grok: write size field: %w", err)
	}
	if err := writer.WriteField("response_format", "url"); err != nil {
		return nil, fmt.Errorf("grok: write response_format field: %w", err)
	}
	if err := writer.WriteField("stream", "false"); err != nil {
		return nil, fmt.Errorf("grok: write stream field: %w", err)
	}

	part, err := writer.CreateFormFile("image", "image.png")
	if err != nil {
		return nil, fmt.Errorf("grok: create form file: %w", err)
	}
	if _, err := part.Write(imageData); err != nil {
		return nil, fmt.Errorf("grok: write image data: %w", err)
	}

	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("grok: close multipart writer: %w", err)
	}

	endpoint := c.baseURL + "/v1/images/edits"
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, &buf)
	if err != nil {
		return nil, fmt.Errorf("grok: create request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("grok: send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("grok: read response: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("grok: API error (status %d): %s", resp.StatusCode, string(respBody))
	}

	imageURL, err := extractEditImageURL(respBody)
	if err != nil {
		return nil, err
	}

	resultData, err := downloadURL(ctx, imageURL)
	if err != nil {
		return nil, fmt.Errorf("grok: download edited image: %w", err)
	}

	if len(resultData) == 0 {
		return nil, fmt.Errorf("grok: downloaded edited image is empty")
	}

	return &ImageResult{ImageData: resultData}, nil
}

// GenerateVideo generates a video from text prompt, optionally with a reference image URL.
func (c *Client) GenerateVideo(ctx context.Context, prompt string, imageURL string) (*VideoResult, error) {
	var content interface{}
	if imageURL != "" {
		content = []map[string]interface{}{
			{"type": "text", "text": prompt},
			{"type": "image_url", "image_url": map[string]string{"url": imageURL}},
		}
	} else {
		content = prompt
	}

	requestBody := map[string]interface{}{
		"model": c.videoModel,
		"messages": []map[string]interface{}{
			{
				"role":    "user",
				"content": content,
			},
		},
		"video_config": map[string]interface{}{
			"aspect_ratio":    "9:16",
			"video_length":    30,
			"resolution_name": "480p",
			"preset":          "custom",
		},
	}

	jsonBody, err := json.Marshal(requestBody)
	if err != nil {
		return nil, fmt.Errorf("grok: marshal request: %w", err)
	}

	endpoint := c.baseURL + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewBuffer(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("grok: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("grok: send request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("grok: read response: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("grok: API error (status %d): %s", resp.StatusCode, string(body))
	}

	videoURL, err := extractVideoURL(body)
	if err != nil {
		return nil, err
	}

	videoData, err := downloadURL(ctx, videoURL)
	if err != nil {
		return nil, fmt.Errorf("grok: download video: %w", err)
	}

	if len(videoData) == 0 {
		return nil, fmt.Errorf("grok: downloaded video is empty")
	}

	return &VideoResult{VideoData: videoData}, nil
}

// extractImageURL extracts the image URL from a chat completions response.
func extractImageURL(body []byte) (string, error) {
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("grok: parse response: %w", err)
	}

	choices, ok := result["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		// Try data array (image generation response format)
		return extractEditImageURL(body)
	}

	choice := choices[0].(map[string]interface{})
	message, ok := choice["message"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("grok: no message in choice")
	}

	content, ok := message["content"].(string)
	if ok && strings.HasPrefix(content, "http") {
		return content, nil
	}

	// Content might be an array with image_url
	contentArr, ok := message["content"].([]interface{})
	if ok {
		for _, item := range contentArr {
			itemMap, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			if itemMap["type"] == "image_url" {
				if imgURL, ok := itemMap["image_url"].(map[string]interface{}); ok {
					if u, ok := imgURL["url"].(string); ok && u != "" {
						return u, nil
					}
				}
			}
		}
	}

	return "", fmt.Errorf("grok: no image URL found in response")
}

// extractEditImageURL extracts the image URL from an images/edits response (data array format).
func extractEditImageURL(body []byte) (string, error) {
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("grok: parse response: %w", err)
	}

	data, ok := result["data"].([]interface{})
	if !ok || len(data) == 0 {
		return "", fmt.Errorf("grok: no data in response")
	}

	first := data[0].(map[string]interface{})
	if u, ok := first["url"].(string); ok && u != "" {
		return u, nil
	}

	return "", fmt.Errorf("grok: no URL in data response")
}

// extractVideoURL extracts the video URL from a chat completions response.
func extractVideoURL(body []byte) (string, error) {
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("grok: parse response: %w", err)
	}

	choices, ok := result["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return "", fmt.Errorf("grok: no choices in response")
	}

	choice := choices[0].(map[string]interface{})
	message, ok := choice["message"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("grok: no message in choice")
	}

	// Video URL might be in content as string
	content, ok := message["content"].(string)
	if ok && strings.HasPrefix(content, "http") {
		return content, nil
	}

	// Or in content array
	contentArr, ok := message["content"].([]interface{})
	if ok {
		for _, item := range contentArr {
			itemMap, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			if itemMap["type"] == "video_url" {
				if vidURL, ok := itemMap["video_url"].(map[string]interface{}); ok {
					if u, ok := vidURL["url"].(string); ok && u != "" {
						return u, nil
					}
				}
			}
			// Also try image_url type (some APIs return video in image_url)
			if itemMap["type"] == "image_url" {
				if imgURL, ok := itemMap["image_url"].(map[string]interface{}); ok {
					if u, ok := imgURL["url"].(string); ok && u != "" {
						return u, nil
					}
				}
			}
		}
	}

	return "", fmt.Errorf("grok: no video URL found in response")
}

// downloadURL downloads content from a URL.
func downloadURL(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, err
	}

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("download failed: status %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}
