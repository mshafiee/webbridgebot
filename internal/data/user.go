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
	var username sql.NullString // Use sql.NullString for nullable columns
	if err := row.Scan(&user.UserID, &user.ChatID, &user.FirstName, &user.LastName, &username, &user.IsAuthorized, &user.IsAdmin, &user.CreatedAt); err != nil {
		return nil, err
	}

	if username.Valid {
		user.Username = username.String
	} else {
		user.Username = ""
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

func (r *UserRepository) DeauthorizeUser(userID int64) error {
	query := `UPDATE users SET is_authorized = 0, is_admin = 0 WHERE user_id = ?`
	_, err := r.db.Exec(query, userID)
	if err != nil {
		return fmt.Errorf("failed to deauthorize user %d: %w", userID, err)
	}
	return nil
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
		var username sql.NullString
		if err := rows.Scan(&user.UserID, &user.ChatID, &user.FirstName, &user.LastName, &username); err != nil {
			return nil, err
		}
		if username.Valid {
			user.Username = username.String
		} else {
			user.Username = ""
		}
		admins = append(admins, user)
	}
	return admins, nil
}

// GetUserCount returns the total number of users in the database.
func (r *UserRepository) GetUserCount() (int, error) {
	query := `SELECT COUNT(*) FROM users`
	var count int
	err := r.db.QueryRow(query).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to get user count: %w", err)
	}
	return count, nil
}

// GetAllUsers retrieves a paginated list of all users.
func (r *UserRepository) GetAllUsers(offset, limit int) ([]User, error) {
	query := `SELECT user_id, chat_id, first_name, last_name, username, is_authorized, is_admin, created_at FROM users ORDER BY user_id LIMIT ? OFFSET ?`
	rows, err := r.db.Query(query, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to query all users: %w", err)
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		var user User
		// Scan into local variables first for username (nullable string)
		var username sql.NullString
		if err := rows.Scan(&user.UserID, &user.ChatID, &user.FirstName, &user.LastName, &username, &user.IsAuthorized, &user.IsAdmin, &user.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan user row: %w", err)
		}
		if username.Valid {
			user.Username = username.String
		} else {
			user.Username = "" // Ensure it's an empty string if null
		}
		users = append(users, user)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error during rows iteration: %w", err)
	}
	return users, nil
}
