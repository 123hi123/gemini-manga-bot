package database

import (
	"database/sql"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Database struct {
	db *sql.DB
}

type SavedPrompt struct {
	ID        int64
	UserID    int64
	Name      string
	Prompt    string
	IsDefault bool
	CreatedAt time.Time
}

type HistoryPrompt struct {
	ID     int64
	UserID int64
	Prompt string
	UsedAt time.Time
}

type UserService struct {
	ID        int64
	UserID    int64
	Name      string
	Type      string
	APIKey    string
	BaseURL   string
	ProjectID string
	Location  string
	Model     string
	IsDefault bool
	CreatedAt time.Time
}

type FailedGeneration struct {
	ID               int64
	UserID           int64
	ChatID           int64
	ReplyToMessageID int64
	Payload          string
	Source           string
	LastError        string
	RetryCount       int
	CreatedAt        time.Time
	LastRetryAt      *time.Time
}

func NewDatabase(dataDir string) (*Database, error) {
	// 確保資料夾存在
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, err
	}

	dbPath := filepath.Join(dataDir, "bot.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}

	d := &Database{db: db}
	if err := d.init(); err != nil {
		return nil, err
	}

	return d, nil
}

func (d *Database) init() error {
	// 建立保存的 Prompt 表
	_, err := d.db.Exec(`
		CREATE TABLE IF NOT EXISTS saved_prompts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			prompt TEXT NOT NULL,
			is_default BOOLEAN DEFAULT FALSE,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(user_id, name)
		)
	`)
	if err != nil {
		return err
	}

	// 建立使用歷史表
	_, err = d.db.Exec(`
		CREATE TABLE IF NOT EXISTS prompt_history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			prompt TEXT NOT NULL,
			used_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)
	`)
	if err != nil {
		return err
	}

	// 建立使用者設定表
	_, err = d.db.Exec(`
		CREATE TABLE IF NOT EXISTS user_settings (
			user_id INTEGER PRIMARY KEY,
			default_quality TEXT DEFAULT '2K',
			default_prompt_id INTEGER,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)
	`)
	if err != nil {
		return err
	}

	// 建立使用者服務設定表
	_, err = d.db.Exec(`
		CREATE TABLE IF NOT EXISTS user_services (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			service_type TEXT NOT NULL,
			api_key TEXT NOT NULL,
			base_url TEXT DEFAULT '',
			project_id TEXT DEFAULT '',
			location TEXT DEFAULT '',
			model TEXT DEFAULT '',
			is_default BOOLEAN DEFAULT FALSE,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(user_id, name)
		)
	`)
	if err != nil {
		return err
	}

	// 建立生成失敗重試佇列表
	_, err = d.db.Exec(`
		CREATE TABLE IF NOT EXISTS failed_generations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			chat_id INTEGER NOT NULL,
			reply_to_message_id INTEGER DEFAULT 0,
			payload TEXT NOT NULL,
			source TEXT DEFAULT 'google',
			last_error TEXT,
			retry_count INTEGER DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			last_retry_at DATETIME
		)
	`)
	if err != nil {
		return err
	}

	// Add source column to existing tables (ignore error if already exists)
	d.db.Exec(`ALTER TABLE failed_generations ADD COLUMN source TEXT DEFAULT 'google'`)

	// 建立使用者圖片佇列表
	_, err = d.db.Exec(`
		CREATE TABLE IF NOT EXISTS user_image_queue (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			chat_id INTEGER NOT NULL,
			file_id TEXT NOT NULL,
			local_path TEXT DEFAULT '',
			ref_count INTEGER DEFAULT 1,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)
	`)
	if err != nil {
		return err
	}

	// Create index on user_image_queue for efficient per-user lookups
	d.db.Exec(`CREATE INDEX IF NOT EXISTS idx_user_image_queue_user ON user_image_queue(user_id, chat_id)`)

	// Create index on failed_generations for efficient per-user lookups
	d.db.Exec(`CREATE INDEX IF NOT EXISTS idx_failed_generations_user ON failed_generations(user_id)`)

	return nil
}

// SavePrompt 保存指定的 Prompt
func (d *Database) SavePrompt(userID int64, name, prompt string) error {
	_, err := d.db.Exec(`
		INSERT OR REPLACE INTO saved_prompts (user_id, name, prompt, created_at)
		VALUES (?, ?, ?, CURRENT_TIMESTAMP)
	`, userID, name, prompt)
	return err
}

// GetSavedPrompts 取得使用者保存的所有 Prompt
func (d *Database) GetSavedPrompts(userID int64) ([]SavedPrompt, error) {
	rows, err := d.db.Query(`
		SELECT id, user_id, name, prompt, is_default, created_at
		FROM saved_prompts
		WHERE user_id = ?
		ORDER BY created_at DESC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var prompts []SavedPrompt
	for rows.Next() {
		var p SavedPrompt
		if err := rows.Scan(&p.ID, &p.UserID, &p.Name, &p.Prompt, &p.IsDefault, &p.CreatedAt); err != nil {
			return nil, err
		}
		prompts = append(prompts, p)
	}
	return prompts, nil
}

// SetDefaultPrompt 設定預設 Prompt
func (d *Database) SetDefaultPrompt(userID int64, promptID int64) error {
	// 先清除其他預設
	_, err := d.db.Exec(`UPDATE saved_prompts SET is_default = FALSE WHERE user_id = ?`, userID)
	if err != nil {
		return err
	}
	// 設定新預設
	_, err = d.db.Exec(`UPDATE saved_prompts SET is_default = TRUE WHERE id = ? AND user_id = ?`, promptID, userID)
	return err
}

// GetDefaultPrompt 取得使用者的預設 Prompt
func (d *Database) GetDefaultPrompt(userID int64) (*SavedPrompt, error) {
	row := d.db.QueryRow(`
		SELECT id, user_id, name, prompt, is_default, created_at
		FROM saved_prompts
		WHERE user_id = ? AND is_default = TRUE
	`, userID)

	var p SavedPrompt
	if err := row.Scan(&p.ID, &p.UserID, &p.Name, &p.Prompt, &p.IsDefault, &p.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &p, nil
}

// AddToHistory 新增到使用歷史
func (d *Database) AddToHistory(userID int64, prompt string) error {
	_, err := d.db.Exec(`
		INSERT INTO prompt_history (user_id, prompt)
		VALUES (?, ?)
	`, userID, prompt)
	return err
}

// GetHistory 取得使用歷史
func (d *Database) GetHistory(userID int64, limit int) ([]HistoryPrompt, error) {
	rows, err := d.db.Query(`
		SELECT id, user_id, prompt, used_at
		FROM prompt_history
		WHERE user_id = ?
		ORDER BY used_at DESC
		LIMIT ?
	`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var history []HistoryPrompt
	for rows.Next() {
		var h HistoryPrompt
		if err := rows.Scan(&h.ID, &h.UserID, &h.Prompt, &h.UsedAt); err != nil {
			return nil, err
		}
		history = append(history, h)
	}
	return history, nil
}

// GetUserSettings 取得使用者設定
func (d *Database) GetUserSettings(userID int64) (string, error) {
	row := d.db.QueryRow(`SELECT default_quality FROM user_settings WHERE user_id = ?`, userID)
	var quality string
	if err := row.Scan(&quality); err != nil {
		if err == sql.ErrNoRows {
			return "2K", nil
		}
		return "", err
	}
	return quality, nil
}

// SetUserSettings 設定使用者預設畫質
func (d *Database) SetUserSettings(userID int64, quality string) error {
	_, err := d.db.Exec(`
		INSERT OR REPLACE INTO user_settings (user_id, default_quality, updated_at)
		VALUES (?, ?, CURRENT_TIMESTAMP)
	`, userID, quality)
	return err
}

// DeletePrompt 刪除保存的 Prompt
func (d *Database) DeletePrompt(userID int64, promptID int64) error {
	_, err := d.db.Exec(`DELETE FROM saved_prompts WHERE id = ? AND user_id = ?`, promptID, userID)
	return err
}

func (d *Database) AddUserService(userID int64, serviceType, name, apiKey, baseURL, projectID, location, model string, setAsDefault bool) (int64, error) {
	tx, err := d.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	if setAsDefault {
		if _, err := tx.Exec(`UPDATE user_services SET is_default = FALSE WHERE user_id = ?`, userID); err != nil {
			return 0, err
		}
	}

	result, err := tx.Exec(`
		INSERT INTO user_services (
			user_id, name, service_type, api_key, base_url, project_id, location, model, is_default, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
	`, userID, name, serviceType, apiKey, baseURL, projectID, location, model, setAsDefault)
	if err != nil {
		return 0, err
	}

	serviceID, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}

	if !setAsDefault {
		var total int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM user_services WHERE user_id = ?`, userID).Scan(&total); err != nil {
			return 0, err
		}
		if total == 1 {
			if _, err := tx.Exec(`UPDATE user_services SET is_default = TRUE WHERE id = ?`, serviceID); err != nil {
				return 0, err
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return serviceID, nil
}

func (d *Database) GetUserServices(userID int64) ([]UserService, error) {
	rows, err := d.db.Query(`
		SELECT id, user_id, name, service_type, api_key, base_url, project_id, location, model, is_default, created_at
		FROM user_services
		WHERE user_id = ?
		ORDER BY is_default DESC, created_at DESC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var services []UserService
	for rows.Next() {
		var service UserService
		if err := rows.Scan(
			&service.ID,
			&service.UserID,
			&service.Name,
			&service.Type,
			&service.APIKey,
			&service.BaseURL,
			&service.ProjectID,
			&service.Location,
			&service.Model,
			&service.IsDefault,
			&service.CreatedAt,
		); err != nil {
			return nil, err
		}
		services = append(services, service)
	}

	return services, nil
}

func (d *Database) GetDefaultUserService(userID int64) (*UserService, error) {
	row := d.db.QueryRow(`
		SELECT id, user_id, name, service_type, api_key, base_url, project_id, location, model, is_default, created_at
		FROM user_services
		WHERE user_id = ? AND is_default = TRUE
		ORDER BY created_at DESC
		LIMIT 1
	`, userID)

	var service UserService
	if err := row.Scan(
		&service.ID,
		&service.UserID,
		&service.Name,
		&service.Type,
		&service.APIKey,
		&service.BaseURL,
		&service.ProjectID,
		&service.Location,
		&service.Model,
		&service.IsDefault,
		&service.CreatedAt,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &service, nil
}

func (d *Database) SetDefaultUserService(userID int64, serviceID int64) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`UPDATE user_services SET is_default = FALSE WHERE user_id = ?`, userID); err != nil {
		return err
	}

	result, err := tx.Exec(`
		UPDATE user_services
		SET is_default = TRUE
		WHERE user_id = ? AND id = ?
	`, userID, serviceID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}

	return tx.Commit()
}

func (d *Database) DeleteUserService(userID int64, serviceID int64) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var wasDefault bool
	if err := tx.QueryRow(`
		SELECT is_default
		FROM user_services
		WHERE user_id = ? AND id = ?
	`, userID, serviceID).Scan(&wasDefault); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}

	if _, err := tx.Exec(`
		DELETE FROM user_services
		WHERE user_id = ? AND id = ?
	`, userID, serviceID); err != nil {
		return err
	}

	if wasDefault {
		var nextID int64
		err := tx.QueryRow(`
			SELECT id
			FROM user_services
			WHERE user_id = ?
			ORDER BY created_at DESC
			LIMIT 1
		`, userID).Scan(&nextID)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == nil {
			if _, err := tx.Exec(`UPDATE user_services SET is_default = TRUE WHERE id = ?`, nextID); err != nil {
				return err
			}
		}
	}

	return tx.Commit()
}

func (d *Database) AddFailedGeneration(userID, chatID, replyToMessageID int64, payload, lastError, source string) error {
	if source == "" {
		source = "google"
	}
	_, err := d.db.Exec(`
		INSERT INTO failed_generations (
			user_id, chat_id, reply_to_message_id, payload, source, last_error, retry_count, created_at
		) VALUES (?, ?, ?, ?, ?, ?, 0, CURRENT_TIMESTAMP)
	`, userID, chatID, replyToMessageID, payload, source, lastError)
	return err
}

func (d *Database) GetRandomFailedGeneration() (*FailedGeneration, error) {
	row := d.db.QueryRow(`
		SELECT id, user_id, chat_id, reply_to_message_id, payload, COALESCE(source, 'google'), last_error, retry_count, created_at, last_retry_at
		FROM failed_generations
		ORDER BY RANDOM()
		LIMIT 1
	`)

	var failed FailedGeneration
	var lastRetry sql.NullTime
	if err := row.Scan(
		&failed.ID,
		&failed.UserID,
		&failed.ChatID,
		&failed.ReplyToMessageID,
		&failed.Payload,
		&failed.Source,
		&failed.LastError,
		&failed.RetryCount,
		&failed.CreatedAt,
		&lastRetry,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}

	if lastRetry.Valid {
		failed.LastRetryAt = &lastRetry.Time
	}

	return &failed, nil
}

func (d *Database) MarkFailedGenerationRetry(id int64, lastError string) error {
	_, err := d.db.Exec(`
		UPDATE failed_generations
		SET retry_count = retry_count + 1,
		    last_error = ?,
		    last_retry_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`, lastError, id)
	return err
}

func (d *Database) DeleteFailedGeneration(id int64) error {
	_, err := d.db.Exec(`DELETE FROM failed_generations WHERE id = ?`, id)
	return err
}

// GetFailedGenerationCounts returns counts of pending retry tasks grouped by source type
// for a specific user.
func (d *Database) GetFailedGenerationCounts(userID int64) (map[string]int, error) {
	rows, err := d.db.Query(`
		SELECT COALESCE(source, 'google'), COUNT(*)
		FROM failed_generations
		WHERE user_id = ?
		GROUP BY COALESCE(source, 'google')
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var source string
		var count int
		if err := rows.Scan(&source, &count); err != nil {
			return nil, err
		}
		counts[source] = count
	}
	return counts, nil
}

func (d *Database) Close() error {
	return d.db.Close()
}

// AddImageToQueue adds an image to a user's image queue.
// If the same file_id already exists for any user/chat, increment ref_count.
func (d *Database) AddImageToQueue(userID, chatID int64, fileID, localPath string) error {
	// Check if same file already exists for this user+chat
	var existing int64
	err := d.db.QueryRow(`
		SELECT id FROM user_image_queue WHERE user_id = ? AND chat_id = ? AND file_id = ?
	`, userID, chatID, fileID).Scan(&existing)
	if err == nil {
		// Already exists, increment ref_count
		_, err = d.db.Exec(`UPDATE user_image_queue SET ref_count = ref_count + 1 WHERE id = ?`, existing)
		return err
	}
	_, err = d.db.Exec(`
		INSERT INTO user_image_queue (user_id, chat_id, file_id, local_path, ref_count, created_at)
		VALUES (?, ?, ?, ?, 1, CURRENT_TIMESTAMP)
	`, userID, chatID, fileID, localPath)
	return err
}

// GetUserImageQueue returns the image queue for a user in a specific chat, ordered by creation time.
func (d *Database) GetUserImageQueue(userID, chatID int64) ([]UserImageQueueItem, error) {
	rows, err := d.db.Query(`
		SELECT id, user_id, chat_id, file_id, local_path, ref_count, created_at
		FROM user_image_queue
		WHERE user_id = ? AND chat_id = ?
		ORDER BY created_at ASC
	`, userID, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []UserImageQueueItem
	for rows.Next() {
		var item UserImageQueueItem
		if err := rows.Scan(&item.ID, &item.UserID, &item.ChatID, &item.FileID, &item.LocalPath, &item.RefCount, &item.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

// ClearUserImageQueue clears the image queue for a user in a specific chat.
// Decrements ref_count; deletes entries with ref_count <= 0.
func (d *Database) ClearUserImageQueue(userID, chatID int64) error {
	_, err := d.db.Exec(`
		UPDATE user_image_queue SET ref_count = ref_count - 1
		WHERE user_id = ? AND chat_id = ?
	`, userID, chatID)
	if err != nil {
		return err
	}
	_, err = d.db.Exec(`DELETE FROM user_image_queue WHERE ref_count <= 0`)
	return err
}

// CleanExpiredImageQueue removes image queue entries older than the given duration.
func (d *Database) CleanExpiredImageQueue(maxAge time.Duration) error {
	cutoff := time.Now().Add(-maxAge)
	_, err := d.db.Exec(`DELETE FROM user_image_queue WHERE created_at < ?`, cutoff)
	return err
}

// DecrementImageRefCount decrements the ref_count of a specific image queue entry.
// Deletes the entry if ref_count reaches zero.
func (d *Database) DecrementImageRefCount(id int64) error {
	_, err := d.db.Exec(`UPDATE user_image_queue SET ref_count = ref_count - 1 WHERE id = ?`, id)
	if err != nil {
		return err
	}
	_, err = d.db.Exec(`DELETE FROM user_image_queue WHERE id = ? AND ref_count <= 0`, id)
	return err
}

// GetAllUserServices returns all services for a user (for rotation).
func (d *Database) GetAllUserServices(userID int64) ([]UserService, error) {
	return d.GetUserServices(userID)
}

// UserImageQueueItem represents an entry in the user's image queue.
type UserImageQueueItem struct {
	ID        int64
	UserID    int64
	ChatID    int64
	FileID    string
	LocalPath string
	RefCount  int
	CreatedAt time.Time
}
