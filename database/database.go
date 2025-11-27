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
	ID        int64
	UserID    int64
	Prompt    string
	UsedAt    time.Time
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
	return err
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

func (d *Database) Close() error {
	return d.db.Close()
}
