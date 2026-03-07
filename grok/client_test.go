package grok

import (
	"encoding/json"
	"testing"
)

func TestNewClient_Defaults(t *testing.T) {
	c := NewClient("test-key", "", "", "", "")
	if c.baseURL != DefaultBaseURL {
		t.Fatalf("expected %s, got %s", DefaultBaseURL, c.baseURL)
	}
	if c.imgModel != DefaultImgModel {
		t.Fatalf("expected %s, got %s", DefaultImgModel, c.imgModel)
	}
	if c.editModel != DefaultEditModel {
		t.Fatalf("expected %s, got %s", DefaultEditModel, c.editModel)
	}
	if c.videoModel != DefaultVideoModel {
		t.Fatalf("expected %s, got %s", DefaultVideoModel, c.videoModel)
	}
}

func TestNewClient_Custom(t *testing.T) {
	c := NewClient("key", "http://custom:9000/", "img-model", "edit-model", "video-model")
	if c.baseURL != "http://custom:9000" {
		t.Fatalf("expected trailing slash removed, got %s", c.baseURL)
	}
	if c.imgModel != "img-model" {
		t.Fatalf("expected img-model, got %s", c.imgModel)
	}
}

func TestAvailable(t *testing.T) {
	c := NewClient("", "", "", "", "")
	if c.Available() {
		t.Fatal("expected not available with empty key")
	}
	c2 := NewClient("key", "", "", "", "")
	if !c2.Available() {
		t.Fatal("expected available with key")
	}
}

func TestExtractImageURL_ChatCompletions(t *testing.T) {
	resp := map[string]interface{}{
		"choices": []interface{}{
			map[string]interface{}{
				"message": map[string]interface{}{
					"content": "https://example.com/image.png",
				},
			},
		},
	}
	body, _ := json.Marshal(resp)
	u, err := extractImageURL(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u != "https://example.com/image.png" {
		t.Fatalf("unexpected URL: %s", u)
	}
}

func TestExtractImageURL_DataArray(t *testing.T) {
	resp := map[string]interface{}{
		"data": []interface{}{
			map[string]interface{}{
				"url": "https://example.com/edited.png",
			},
		},
	}
	body, _ := json.Marshal(resp)
	u, err := extractImageURL(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u != "https://example.com/edited.png" {
		t.Fatalf("unexpected URL: %s", u)
	}
}

func TestExtractEditImageURL(t *testing.T) {
	resp := map[string]interface{}{
		"data": []interface{}{
			map[string]interface{}{
				"url": "https://example.com/result.png",
			},
		},
	}
	body, _ := json.Marshal(resp)
	u, err := extractEditImageURL(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u != "https://example.com/result.png" {
		t.Fatalf("unexpected URL: %s", u)
	}
}

func TestExtractVideoURL(t *testing.T) {
	resp := map[string]interface{}{
		"choices": []interface{}{
			map[string]interface{}{
				"message": map[string]interface{}{
					"content": "https://example.com/video.mp4",
				},
			},
		},
	}
	body, _ := json.Marshal(resp)
	u, err := extractVideoURL(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u != "https://example.com/video.mp4" {
		t.Fatalf("unexpected URL: %s", u)
	}
}

func TestExtractImageURL_NoData(t *testing.T) {
	body := []byte(`{}`)
	_, err := extractImageURL(body)
	if err == nil {
		t.Fatal("expected error for empty response")
	}
}

func TestExtractVideoURL_ContentArray(t *testing.T) {
	resp := map[string]interface{}{
		"choices": []interface{}{
			map[string]interface{}{
				"message": map[string]interface{}{
					"content": []interface{}{
						map[string]interface{}{
							"type": "image_url",
							"image_url": map[string]interface{}{
								"url": "https://example.com/video2.mp4",
							},
						},
					},
				},
			},
		},
	}
	body, _ := json.Marshal(resp)
	u, err := extractVideoURL(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u != "https://example.com/video2.mp4" {
		t.Fatalf("unexpected URL: %s", u)
	}
}
