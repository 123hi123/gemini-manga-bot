package gemini

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	apiKey      string
	baseURL     string
	serviceType string
	projectID   string
	location    string
	imageModel  string
	textModel   string
	ttsModel    string
	httpClient  *http.Client
}

const (
	ServiceTypeStandard = "standard"
	ServiceTypeCustom   = "custom"
	ServiceTypeVertex   = "vertex"

	DefaultGeminiBaseURL = "https://generativelanguage.googleapis.com"
	DefaultVertexBaseURL = "https://aiplatform.googleapis.com"

	DefaultImageModel = "gemini-3-pro-image-preview"
	DefaultTextModel  = "gemini-2.5-flash"
	DefaultTTSModel   = "gemini-2.5-flash-preview-tts"
)

type ServiceConfig struct {
	Type      string `json:"type"`
	Name      string `json:"name,omitempty"`
	APIKey    string `json:"api_key"`
	BaseURL   string `json:"base_url,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
	Location  string `json:"location,omitempty"`
	Model     string `json:"model,omitempty"`
}

type ImageResult struct {
	ImageData []byte
	Text      string
}

type TTSResult struct {
	AudioData []byte
}

type ImageInfo struct {
	Width       int
	Height      int
	AspectRatio string // 匹配的比例，如 "16:9"，或空字串表示讓模型自動決定
}

// 支援的比例列表
var supportedRatios = []struct {
	Name  string
	Ratio float64
}{
	{"1:1", 1.0},
	{"2:3", 2.0 / 3.0},
	{"3:2", 3.0 / 2.0},
	{"3:4", 3.0 / 4.0},
	{"4:3", 4.0 / 3.0},
	{"4:5", 4.0 / 5.0},
	{"5:4", 5.0 / 4.0},
	{"9:16", 9.0 / 16.0},
	{"16:9", 16.0 / 9.0},
	{"21:9", 21.0 / 9.0},
}

func NewClient(apiKey string) *Client {
	return &Client{
		apiKey:      apiKey,
		baseURL:     DefaultGeminiBaseURL,
		serviceType: ServiceTypeStandard,
		imageModel:  DefaultImageModel,
		textModel:   DefaultTextModel,
		ttsModel:    DefaultTTSModel,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

func NewClientWithService(service ServiceConfig) *Client {
	serviceType := normalizeServiceType(service.Type)
	baseURL := strings.TrimSpace(service.BaseURL)
	if baseURL == "" {
		if serviceType == ServiceTypeVertex {
			baseURL = DefaultVertexBaseURL
		} else {
			baseURL = DefaultGeminiBaseURL
		}
	}

	model := strings.TrimSpace(service.Model)
	if model == "" {
		model = DefaultImageModel
	}

	return &Client{
		apiKey:      service.APIKey,
		baseURL:     baseURL,
		serviceType: serviceType,
		projectID:   strings.TrimSpace(service.ProjectID),
		location:    strings.TrimSpace(service.Location),
		imageModel:  model,
		textModel:   DefaultTextModel,
		ttsModel:    DefaultTTSModel,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

// GetImageInfo 取得圖片資訊並計算最接近的支援比例
func GetImageInfo(imageData []byte) (*ImageInfo, error) {
	reader := bytes.NewReader(imageData)
	config, _, err := image.DecodeConfig(reader)
	if err != nil {
		return nil, err
	}

	info := &ImageInfo{
		Width:  config.Width,
		Height: config.Height,
	}

	// 計算實際比例
	actualRatio := float64(config.Width) / float64(config.Height)

	// 找最接近的支援比例
	minDiff := math.MaxFloat64
	matchedRatio := ""

	for _, r := range supportedRatios {
		diff := math.Abs(actualRatio - r.Ratio)
		if diff < minDiff {
			minDiff = diff
			matchedRatio = r.Name
		}
	}

	// 如果差異太大（超過 10%），就讓模型自動決定
	threshold := 0.1
	if minDiff/actualRatio > threshold {
		info.AspectRatio = "" // 讓模型自動決定
	} else {
		info.AspectRatio = matchedRatio
	}

	return info, nil
}

// GenerateImage 生成翻譯後的漫畫圖片
func (c *Client) GenerateImage(ctx context.Context, imageData []byte, mimeType, prompt, quality, aspectRatio string) (*ImageResult, error) {
	imageBase64 := base64.StdEncoding.EncodeToString(imageData)

	// 建立 imageConfig
	imageConfig := map[string]interface{}{
		"imageSize": quality,
	}

	// 只有當 aspectRatio 不為空時才設定
	if aspectRatio != "" {
		imageConfig["aspectRatio"] = aspectRatio
	}

	requestBody := map[string]interface{}{
		"contents": []map[string]interface{}{
			{
				"role": "user",
				"parts": []map[string]interface{}{
					{"text": prompt},
					{
						"inline_data": map[string]string{
							"mime_type": mimeType,
							"data":      imageBase64,
						},
					},
				},
			},
		},
		"generationConfig": map[string]interface{}{
			"responseModalities": []string{"IMAGE"},
			"imageConfig":        imageConfig,
		},
		"safetySettings": []map[string]interface{}{
			{"category": "HARM_CATEGORY_HATE_SPEECH", "threshold": "OFF"},
			{"category": "HARM_CATEGORY_HARASSMENT", "threshold": "OFF"},
			{"category": "HARM_CATEGORY_SEXUALLY_EXPLICIT", "threshold": "OFF"},
			{"category": "HARM_CATEGORY_DANGEROUS_CONTENT", "threshold": "OFF"},
		},
	}

	jsonBody, err := json.Marshal(requestBody)
	if err != nil {
		return nil, err
	}

	url, err := c.buildGenerateURL(c.imageModel)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(jsonBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API error: %s", string(body))
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}

	// 解析回應取得圖片
	candidates, ok := result["candidates"].([]interface{})
	if !ok || len(candidates) == 0 {
		return nil, fmt.Errorf("no candidates in response")
	}

	candidate := candidates[0].(map[string]interface{})
	content, ok := candidate["content"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("no content in candidate")
	}

	parts, ok := content["parts"].([]interface{})
	if !ok || len(parts) == 0 {
		return nil, fmt.Errorf("no parts in content")
	}

	for _, part := range parts {
		partMap := part.(map[string]interface{})
		if inlineData, ok := partMap["inlineData"].(map[string]interface{}); ok {
			if dataStr, ok := inlineData["data"].(string); ok {
				imageBytes, err := base64.StdEncoding.DecodeString(dataStr)
				if err != nil {
					return nil, err
				}
				return &ImageResult{ImageData: imageBytes}, nil
			}
		}
	}

	return nil, fmt.Errorf("no image data in response")
}

// DownloadedImage 下載的圖片資料
type DownloadedImage struct {
	Data     []byte
	MimeType string
}

// GenerateImageWithContext 使用多張圖片作為上下文生成圖片
func (c *Client) GenerateImageWithContext(ctx context.Context, images []DownloadedImage, prompt, quality, aspectRatio string) (*ImageResult, error) {
	// 建立 parts
	var parts []map[string]interface{}

	// 先加入文字 prompt
	parts = append(parts, map[string]interface{}{"text": prompt})

	// 加入所有圖片
	for _, img := range images {
		imageBase64 := base64.StdEncoding.EncodeToString(img.Data)
		parts = append(parts, map[string]interface{}{
			"inline_data": map[string]string{
				"mime_type": img.MimeType,
				"data":      imageBase64,
			},
		})
	}

	// 建立 imageConfig
	imageConfig := map[string]interface{}{
		"imageSize": quality,
	}

	// 只有當 aspectRatio 不為空時才設定
	if aspectRatio != "" {
		imageConfig["aspectRatio"] = aspectRatio
	}

	requestBody := map[string]interface{}{
		"contents": []map[string]interface{}{
			{
				"role":  "user",
				"parts": parts,
			},
		},
		"generationConfig": map[string]interface{}{
			"responseModalities": []string{"IMAGE"},
			"imageConfig":        imageConfig,
		},
		"safetySettings": []map[string]interface{}{
			{"category": "HARM_CATEGORY_HATE_SPEECH", "threshold": "OFF"},
			{"category": "HARM_CATEGORY_HARASSMENT", "threshold": "OFF"},
			{"category": "HARM_CATEGORY_SEXUALLY_EXPLICIT", "threshold": "OFF"},
			{"category": "HARM_CATEGORY_DANGEROUS_CONTENT", "threshold": "OFF"},
		},
	}

	return c.sendImageRequest(ctx, requestBody)
}

// GenerateImageFromText 純文字生成圖片
func (c *Client) GenerateImageFromText(ctx context.Context, prompt, quality, aspectRatio string) (*ImageResult, error) {
	// 建立 imageConfig
	imageConfig := map[string]interface{}{
		"imageSize": quality,
	}

	// 只有當 aspectRatio 不為空時才設定
	if aspectRatio != "" {
		imageConfig["aspectRatio"] = aspectRatio
	}

	requestBody := map[string]interface{}{
		"contents": []map[string]interface{}{
			{
				"role": "user",
				"parts": []map[string]interface{}{
					{"text": prompt},
				},
			},
		},
		"generationConfig": map[string]interface{}{
			"responseModalities": []string{"IMAGE"},
			"imageConfig":        imageConfig,
		},
		"safetySettings": []map[string]interface{}{
			{"category": "HARM_CATEGORY_HATE_SPEECH", "threshold": "OFF"},
			{"category": "HARM_CATEGORY_HARASSMENT", "threshold": "OFF"},
			{"category": "HARM_CATEGORY_SEXUALLY_EXPLICIT", "threshold": "OFF"},
			{"category": "HARM_CATEGORY_DANGEROUS_CONTENT", "threshold": "OFF"},
		},
	}

	return c.sendImageRequest(ctx, requestBody)
}

// sendImageRequest 發送圖片生成請求的共用函式
func (c *Client) sendImageRequest(ctx context.Context, requestBody map[string]interface{}) (*ImageResult, error) {
	jsonBody, err := json.Marshal(requestBody)
	if err != nil {
		return nil, err
	}

	url, err := c.buildGenerateURL(c.imageModel)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(jsonBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API error: %s", string(body))
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}

	// 解析回應取得圖片
	candidates, ok := result["candidates"].([]interface{})
	if !ok || len(candidates) == 0 {
		return nil, fmt.Errorf("no candidates in response")
	}

	candidate := candidates[0].(map[string]interface{})
	content, ok := candidate["content"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("no content in candidate")
	}

	parts, ok := content["parts"].([]interface{})
	if !ok || len(parts) == 0 {
		return nil, fmt.Errorf("no parts in content")
	}

	for _, part := range parts {
		partMap := part.(map[string]interface{})
		if inlineData, ok := partMap["inlineData"].(map[string]interface{}); ok {
			if dataStr, ok := inlineData["data"].(string); ok {
				imageBytes, err := base64.StdEncoding.DecodeString(dataStr)
				if err != nil {
					return nil, err
				}
				return &ImageResult{ImageData: imageBytes}, nil
			}
		}
	}

	return nil, fmt.Errorf("no image data in response")
}

// ExtractText 從圖片擷取文字
func (c *Client) ExtractText(ctx context.Context, imageData []byte, mimeType, prompt string) (string, error) {
	imageBase64 := base64.StdEncoding.EncodeToString(imageData)

	requestBody := map[string]interface{}{
		"contents": []map[string]interface{}{
			{
				"role": "user",
				"parts": []map[string]interface{}{
					{"text": prompt},
					{
						"inline_data": map[string]string{
							"mime_type": mimeType,
							"data":      imageBase64,
						},
					},
				},
			},
		},
	}

	jsonBody, err := json.Marshal(requestBody)
	if err != nil {
		return "", err
	}

	url, err := c.buildGenerateURL(c.textModel)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(jsonBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("API error: %s", string(body))
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", err
	}

	// 解析文字回應
	candidates, ok := result["candidates"].([]interface{})
	if !ok || len(candidates) == 0 {
		return "", fmt.Errorf("no candidates in response")
	}

	candidate := candidates[0].(map[string]interface{})
	content, ok := candidate["content"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("no content in candidate")
	}

	parts, ok := content["parts"].([]interface{})
	if !ok || len(parts) == 0 {
		return "", fmt.Errorf("no parts in content")
	}

	for _, part := range parts {
		partMap := part.(map[string]interface{})
		if text, ok := partMap["text"].(string); ok {
			return text, nil
		}
	}

	return "", fmt.Errorf("no text in response")
}

// GenerateTTS 生成語音
func (c *Client) GenerateTTS(ctx context.Context, text, voiceName string) (*TTSResult, error) {
	requestBody := map[string]interface{}{
		"contents": []map[string]interface{}{
			{
				"role": "user",
				"parts": []map[string]interface{}{
					{"text": fmt.Sprintf("请用自然的语气朗读以下漫画对话内容：\n\n%s", text)},
				},
			},
		},
		"generationConfig": map[string]interface{}{
			"responseModalities": []string{"AUDIO"},
			"speechConfig": map[string]interface{}{
				"voiceConfig": map[string]interface{}{
					"prebuiltVoiceConfig": map[string]string{
						"voiceName": voiceName,
					},
				},
			},
		},
	}

	jsonBody, err := json.Marshal(requestBody)
	if err != nil {
		return nil, err
	}

	url, err := c.buildGenerateURL(c.ttsModel)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(jsonBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API error: %s", string(body))
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}

	// 解析音訊回應
	candidates, ok := result["candidates"].([]interface{})
	if !ok || len(candidates) == 0 {
		return nil, fmt.Errorf("no candidates in response")
	}

	candidate := candidates[0].(map[string]interface{})
	content, ok := candidate["content"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("no content in candidate")
	}

	parts, ok := content["parts"].([]interface{})
	if !ok || len(parts) == 0 {
		return nil, fmt.Errorf("no parts in content")
	}

	for _, part := range parts {
		partMap := part.(map[string]interface{})
		if inlineData, ok := partMap["inlineData"].(map[string]interface{}); ok {
			if dataStr, ok := inlineData["data"].(string); ok {
				audioBytes, err := base64.StdEncoding.DecodeString(dataStr)
				if err != nil {
					return nil, err
				}
				return &TTSResult{AudioData: audioBytes}, nil
			}
		}
	}

	return nil, fmt.Errorf("no audio data in response")
}

func normalizeServiceType(serviceType string) string {
	normalized := strings.ToLower(strings.TrimSpace(serviceType))
	switch normalized {
	case "", "gemini", "standard", "origin", "original":
		return ServiceTypeStandard
	case ServiceTypeCustom:
		return ServiceTypeCustom
	case ServiceTypeVertex, "gcp":
		return ServiceTypeVertex
	default:
		return ServiceTypeStandard
	}
}

func (c *Client) buildGenerateURL(model string) (string, error) {
	if strings.TrimSpace(c.apiKey) == "" {
		return "", fmt.Errorf("service api key is empty")
	}

	baseURL := strings.TrimSpace(c.baseURL)
	serviceType := normalizeServiceType(c.serviceType)
	model = strings.TrimSpace(model)
	if model == "" {
		model = DefaultImageModel
	}

	// 允許直接填完整 generateContent endpoint
	if strings.Contains(baseURL, ":generateContent") {
		return appendAPIKey(baseURL, c.apiKey)
	}

	if serviceType == ServiceTypeVertex {
		projectID := strings.TrimSpace(c.projectID)
		location := strings.TrimSpace(c.location)
		// Vertex Express Mode：只需 API Key（不需 project/location）
		if projectID == "" || location == "" {
			if baseURL == "" {
				baseURL = DefaultVertexBaseURL
			}
			endpoint := fmt.Sprintf(
				"%s/v1/publishers/google/models/%s:generateContent",
				strings.TrimRight(baseURL, "/"),
				url.PathEscape(model),
			)
			return appendAPIKey(endpoint, c.apiKey)
		}

		// Vertex Full Mode：project/location
		if baseURL == "" {
			baseURL = fmt.Sprintf("https://%s-aiplatform.googleapis.com", location)
		}
		endpoint := fmt.Sprintf(
			"%s/v1/projects/%s/locations/%s/publishers/google/models/%s:generateContent",
			strings.TrimRight(baseURL, "/"),
			url.PathEscape(projectID),
			url.PathEscape(location),
			url.PathEscape(model),
		)
		return appendAPIKey(endpoint, c.apiKey)
	}

	if baseURL == "" {
		baseURL = DefaultGeminiBaseURL
	}
	endpoint := fmt.Sprintf(
		"%s/v1beta/models/%s:generateContent",
		strings.TrimRight(baseURL, "/"),
		url.PathEscape(model),
	)
	return appendAPIKey(endpoint, c.apiKey)
}

func appendAPIKey(rawURL, apiKey string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}

	query := parsed.Query()
	query.Set("key", apiKey)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}
