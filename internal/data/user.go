package data

import (
	"database/sql"
	"fmt"
)

type User struct {
	UserID       int64
	ChatID       int64
	FirstName    string
	LastName     string
	Username     string
	IsAuthorized bool
	IsAdmin      bool
	CreatedAt    string
}

type UserRepository struct {
	db *sql.DB
}

// NewUserRepository creates a new instance of UserRepository.
func NewUserRepository(db *sql.DB) *UserRepository {
	return &UserRepository{db: db}
}

// InitDB initializes the database by creating necessary tables.
func (r *UserRepository) InitDB() error {
	query := `
	CREATE TABLE IF NOT EXISTS users (
		user_id INTEGER PRIMARY KEY,
		chat_id INTEGER NOT NULL,
		first_name TEXT,
		last_name TEXT,
		username TEXT,
		is_authorized BOOLEAN DEFAULT FALSE,
		is_admin BOOLEAN DEFAULT FALSE,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);`

	_, err := r.db.Exec(query)
	if err != nil {
		return fmt.Errorf("failed to create users table: %w", err)
	}

	return nil
}

// StoreUserInfo stores or updates user information in the database.
func (r *UserRepository) StoreUserInfo(userID, chatID int64, firstName, lastName, username string, isAuthorized, isAdmin bool) error {
	query := `
	INSERT INTO users (user_id, chat_id, first_name, last_name, username, is_authorized, is_admin)
	VALUES (?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(user_id) DO UPDATE SET
	chat_id=excluded.chat_id,
	first_name=excluded.first_name,
	last_name=excluded.last_name,
	username=excluded.username,
	is_authorized=excluded.is_authorized,
	is_admin=excluded.is_admin;
	`

	_, err := r.db.Exec(query, userID, chatID, firstName, lastName, username, isAuthorized, isAdmin)
	return err
}

// GetUserInfo retrieves user information from the database by user ID.
func (r *UserRepository) GetUserInfo(userID int64) (*User, error) {
	query := `SELECT user_id, chat_id, first_name, last_name, username, is_authorized, is_admin, created_at FROM users WHERE user_id = ?`
	row := r.db.QueryRow(query, userID)

	var user User
	if err := row.Scan(&user.UserID, &user.ChatID, &user.FirstName, &user.LastName, &user.Username, &user.IsAuthorized, &user.IsAdmin, &user.CreatedAt); err != nil {
		return nil, err
	}

	return &user, nil
}

// IsFirstUser checks if the current user is the first user in the database.
func (r *UserRepository) IsFirstUser() (bool, error) {
	query := `SELECT COUNT(*) FROM users`
	var count int
	err := r.db.QueryRow(query).Scan(&count)
	if err != nil {
		return false, err
	}
	return count == 0, nil
}

// AuthorizeUser sets the is_authorized and optionally is_admin flags for a given user.
func (r *UserRepository) AuthorizeUser(userID int64, isAdmin bool) error {
	query := `UPDATE users SET is_authorized = TRUE, is_admin = ? WHERE user_id = ?`
	_, err := r.db.Exec(query, isAdmin, userID)
	return err
}

// GetAllAdmins retrieves a list of all admin users.
func (r *UserRepository) GetAllAdmins() ([]User, error) {
	query := `SELECT user_id, chat_id, first_name, last_name, username FROM users WHERE is_admin = TRUE`
	rows, err := r.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var admins []User
	for rows.Next() {
		var user User
		if err := rows.Scan(&user.UserID, &user.ChatID, &user.FirstName, &user.LastName, &user.Username); err != nil {
			return nil, err
		}
		admins = append(admins, user)
	}
	return admins, nil
}
