package gemini

import (
	"strings"
	"testing"
)

func TestBuildGenerateURL_Standard(t *testing.T) {
	client := NewClientWithService(ServiceConfig{
		Type:   ServiceTypeStandard,
		APIKey: "abc123",
	})

	url, err := client.buildGenerateURL(DefaultImageModel)
	if err != nil {
		t.Fatalf("buildGenerateURL standard failed: %v", err)
	}
	if !strings.Contains(url, "generativelanguage.googleapis.com") {
		t.Fatalf("expected Gemini endpoint, got: %s", url)
	}
	if !strings.Contains(url, "key=abc123") {
		t.Fatalf("expected api key in query, got: %s", url)
	}
}

func TestBuildGenerateURL_Custom(t *testing.T) {
	client := NewClientWithService(ServiceConfig{
		Type:    ServiceTypeCustom,
		APIKey:  "abc123",
		BaseURL: "https://proxy.example.com",
	})

	url, err := client.buildGenerateURL(DefaultImageModel)
	if err != nil {
		t.Fatalf("buildGenerateURL custom failed: %v", err)
	}
	if !strings.Contains(url, "proxy.example.com/v1beta/models/") {
		t.Fatalf("expected custom endpoint, got: %s", url)
	}
}

func TestBuildGenerateURL_Vertex(t *testing.T) {
	client := NewClientWithService(ServiceConfig{
		Type:      ServiceTypeVertex,
		APIKey:    "abc123",
		ProjectID: "proj",
		Location:  "asia-east1",
	})

	url, err := client.buildGenerateURL(DefaultImageModel)
	if err != nil {
		t.Fatalf("buildGenerateURL vertex failed: %v", err)
	}
	if !strings.Contains(url, "/projects/proj/locations/asia-east1/publishers/google/models/") {
		t.Fatalf("expected vertex endpoint, got: %s", url)
	}
}

func TestBuildGenerateURL_VertexExpressMode(t *testing.T) {
	client := NewClientWithService(ServiceConfig{
		Type:   ServiceTypeVertex,
		APIKey: "abc123",
	})

	url, err := client.buildGenerateURL(DefaultImageModel)
	if err != nil {
		t.Fatalf("buildGenerateURL vertex express failed: %v", err)
	}
	if !strings.Contains(url, "aiplatform.googleapis.com/v1/publishers/google/models/") {
		t.Fatalf("expected vertex express endpoint, got: %s", url)
	}
}
