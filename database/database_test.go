package database

import "testing"

func TestUserServiceCRUD(t *testing.T) {
	db, err := NewDatabase(t.TempDir())
	if err != nil {
		t.Fatalf("NewDatabase failed: %v", err)
	}
	defer db.Close()

	firstID, err := db.AddUserService(1, "standard", "main", "key1", "", "", "", "", true)
	if err != nil {
		t.Fatalf("AddUserService first failed: %v", err)
	}
	secondID, err := db.AddUserService(1, "custom", "proxy", "key2", "https://example.com", "", "", "", false)
	if err != nil {
		t.Fatalf("AddUserService second failed: %v", err)
	}

	defaultService, err := db.GetDefaultUserService(1)
	if err != nil {
		t.Fatalf("GetDefaultUserService failed: %v", err)
	}
	if defaultService == nil || defaultService.ID != firstID {
		t.Fatalf("expected default id %d, got %+v", firstID, defaultService)
	}

	if err := db.SetDefaultUserService(1, secondID); err != nil {
		t.Fatalf("SetDefaultUserService failed: %v", err)
	}
	defaultService, err = db.GetDefaultUserService(1)
	if err != nil {
		t.Fatalf("GetDefaultUserService after switch failed: %v", err)
	}
	if defaultService == nil || defaultService.ID != secondID {
		t.Fatalf("expected default id %d after switch, got %+v", secondID, defaultService)
	}

	if err := db.DeleteUserService(1, secondID); err != nil {
		t.Fatalf("DeleteUserService failed: %v", err)
	}
	defaultService, err = db.GetDefaultUserService(1)
	if err != nil {
		t.Fatalf("GetDefaultUserService after delete failed: %v", err)
	}
	if defaultService == nil || defaultService.ID != firstID {
		t.Fatalf("expected fallback default id %d, got %+v", firstID, defaultService)
	}
}

func TestFailedGenerationQueue(t *testing.T) {
	db, err := NewDatabase(t.TempDir())
	if err != nil {
		t.Fatalf("NewDatabase failed: %v", err)
	}
	defer db.Close()

	if err := db.AddFailedGeneration(10, 20, 30, `{"prompt":"x"}`, "boom", "google"); err != nil {
		t.Fatalf("AddFailedGeneration failed: %v", err)
	}

	task, err := db.GetRandomFailedGeneration()
	if err != nil {
		t.Fatalf("GetRandomFailedGeneration failed: %v", err)
	}
	if task == nil {
		t.Fatalf("expected one failed generation task")
	}
	if task.UserID != 10 || task.ChatID != 20 || task.ReplyToMessageID != 30 {
		t.Fatalf("unexpected task: %+v", task)
	}

	if err := db.MarkFailedGenerationRetry(task.ID, "still boom"); err != nil {
		t.Fatalf("MarkFailedGenerationRetry failed: %v", err)
	}

	task, err = db.GetRandomFailedGeneration()
	if err != nil {
		t.Fatalf("GetRandomFailedGeneration second read failed: %v", err)
	}
	if task == nil || task.RetryCount != 1 {
		t.Fatalf("expected retry_count=1, got %+v", task)
	}
	if task.Source != "google" {
		t.Fatalf("expected source=google, got %s", task.Source)
	}

	if err := db.DeleteFailedGeneration(task.ID); err != nil {
		t.Fatalf("DeleteFailedGeneration failed: %v", err)
	}
	task, err = db.GetRandomFailedGeneration()
	if err != nil {
		t.Fatalf("GetRandomFailedGeneration after delete failed: %v", err)
	}
	if task != nil {
		t.Fatalf("expected empty queue, got %+v", task)
	}
}

func TestImageQueue(t *testing.T) {
	db, err := NewDatabase(t.TempDir())
	if err != nil {
		t.Fatalf("NewDatabase failed: %v", err)
	}
	defer db.Close()

	// Add images to queue
	if err := db.AddImageToQueue(1, 100, "file1", ""); err != nil {
		t.Fatalf("AddImageToQueue failed: %v", err)
	}
	if err := db.AddImageToQueue(1, 100, "file2", ""); err != nil {
		t.Fatalf("AddImageToQueue second failed: %v", err)
	}

	// Duplicate should increment ref_count
	if err := db.AddImageToQueue(1, 100, "file1", ""); err != nil {
		t.Fatalf("AddImageToQueue duplicate failed: %v", err)
	}

	items, err := db.GetUserImageQueue(1, 100)
	if err != nil {
		t.Fatalf("GetUserImageQueue failed: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}

	// file1 should have ref_count=2
	if items[0].FileID != "file1" || items[0].RefCount != 2 {
		t.Fatalf("expected file1 with ref_count=2, got %+v", items[0])
	}

	// Clear queue (decrements ref_count)
	if err := db.ClearUserImageQueue(1, 100); err != nil {
		t.Fatalf("ClearUserImageQueue failed: %v", err)
	}

	items, err = db.GetUserImageQueue(1, 100)
	if err != nil {
		t.Fatalf("GetUserImageQueue after clear failed: %v", err)
	}
	// file1 should still exist with ref_count=1
	if len(items) != 1 {
		t.Fatalf("expected 1 item after clear, got %d", len(items))
	}
	if items[0].FileID != "file1" || items[0].RefCount != 1 {
		t.Fatalf("expected file1 with ref_count=1, got %+v", items[0])
	}
}
