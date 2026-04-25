package service

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"nimbus/internal/db"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

var (
	ErrEmailTaken          = errors.New("email already registered")
	ErrInvalidCredentials  = errors.New("invalid credentials")
	ErrSessionNotFound     = errors.New("session not found or expired")
	ErrAdminAlreadyClaimed = errors.New("admin already claimed")
	ErrUsersExist          = errors.New("users already exist")
)

const sessionDuration = 7 * 24 * time.Hour

// AuthService handles account creation and credential verification.
type AuthService struct {
	db *db.DB
}

// NewAuthService creates a new AuthService.
func NewAuthService(database *db.DB) *AuthService {
	return &AuthService{db: database}
}

// RegisterParams holds the input for creating a new account.
type RegisterParams struct {
	Name     string
	Email    string
	Password string
}

// UserView is the safe, serialisable projection of a User (no password hash).
type UserView struct {
	ID      uint   `json:"id"`
	Name    string `json:"name"`
	Email   string `json:"email"`
	IsAdmin bool   `json:"is_admin"`
}

func userToView(u *db.User) *UserView {
	return &UserView{ID: u.ID, Name: u.Name, Email: u.Email, IsAdmin: u.IsAdmin}
}

// Register creates a new user account with a bcrypt-hashed password.
// Returns ErrEmailTaken if the email is already in use.
func (s *AuthService) Register(p RegisterParams) (*UserView, error) {
	email := strings.ToLower(strings.TrimSpace(p.Email))

	var existing db.User
	err := s.db.Where("email = ?", email).First(&existing).Error
	if err == nil {
		return nil, ErrEmailTaken
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(p.Password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}

	user := &db.User{
		Name:         strings.TrimSpace(p.Name),
		Email:        email,
		PasswordHash: string(hash),
	}
	if err := s.db.Create(user).Error; err != nil {
		return nil, err
	}

	return userToView(user), nil
}

// Login verifies credentials and returns a new session ID.
// Returns ErrInvalidCredentials if the email is not found or the password is wrong.
func (s *AuthService) Login(email, password string) (string, *UserView, error) {
	email = strings.ToLower(strings.TrimSpace(email))

	var user db.User
	if err := s.db.Where("email = ?", email).First(&user).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", nil, ErrInvalidCredentials
		}
		return "", nil, err
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return "", nil, ErrInvalidCredentials
	}

	sessionID, err := s.CreateSession(user.ID)
	if err != nil {
		return "", nil, err
	}

	return sessionID, userToView(&user), nil
}

// UpsertOAuthUser finds a user by email or creates one if none exists.
// Used by OAuth providers where a password hash is not applicable.
func (s *AuthService) UpsertOAuthUser(name, email string) (*UserView, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	name = strings.TrimSpace(name)

	var user db.User
	err := s.db.Where("email = ?", email).First(&user).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		user = db.User{Name: name, Email: email}
		if err := s.db.Create(&user).Error; err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}

	return userToView(&user), nil
}

// CreateSession creates a new session for the given user ID and returns the session ID.
func (s *AuthService) CreateSession(userID uint) (string, error) {
	id, err := generateSessionID()
	if err != nil {
		return "", err
	}

	session := &db.Session{
		ID:        id,
		UserID:    userID,
		ExpiresAt: time.Now().Add(sessionDuration),
	}
	if err := s.db.Create(session).Error; err != nil {
		return "", err
	}

	return id, nil
}

// GetUserBySessionID returns the user for a valid, non-expired session.
func (s *AuthService) GetUserBySessionID(sessionID string) (*UserView, error) {
	var session db.Session
	err := s.db.Where("id = ? AND expires_at > ?", sessionID, time.Now()).First(&session).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, err
	}

	var user db.User
	if err := s.db.First(&user, session.UserID).Error; err != nil {
		return nil, err
	}

	return userToView(&user), nil
}

// Logout deletes the session record.
func (s *AuthService) Logout(sessionID string) error {
	return s.db.Delete(&db.Session{}, "id = ?", sessionID).Error
}

func generateSessionID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// RegisterFirstAdmin creates the first admin account. Returns ErrUsersExist if
// any user already exists — only valid for the initial setup wizard.
func (s *AuthService) RegisterFirstAdmin(p RegisterParams) (*UserView, error) {
	has, err := s.HasAnyUsers()
	if err != nil {
		return nil, err
	}
	if has {
		return nil, ErrUsersExist
	}

	email := strings.ToLower(strings.TrimSpace(p.Email))
	hash, err := bcrypt.GenerateFromPassword([]byte(p.Password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}

	user := &db.User{
		Name:         strings.TrimSpace(p.Name),
		Email:        email,
		PasswordHash: string(hash),
		IsAdmin:      true,
	}
	if err := s.db.Create(user).Error; err != nil {
		return nil, err
	}
	return userToView(user), nil
}

// HasAnyUsers returns true if at least one user account exists.
func (s *AuthService) HasAnyUsers() (bool, error) {
	var count int64
	err := s.db.Model(&db.User{}).Count(&count).Error
	return count > 0, err
}

// IsAdminClaimed returns true if at least one admin account exists.
func (s *AuthService) IsAdminClaimed() (bool, error) {
	var count int64
	err := s.db.Model(&db.User{}).Where("is_admin = ?", true).Count(&count).Error
	return count > 0, err
}

// ClaimAdmin promotes the given user to admin. Returns ErrAdminAlreadyClaimed
// if any admin already exists.
func (s *AuthService) ClaimAdmin(userID uint) error {
	claimed, err := s.IsAdminClaimed()
	if err != nil {
		return err
	}
	if claimed {
		return ErrAdminAlreadyClaimed
	}
	return s.db.Model(&db.User{}).Where("id = ?", userID).Update("is_admin", true).Error
}

// ListAllUsers returns a view of every registered account.
func (s *AuthService) ListAllUsers() ([]*UserView, error) {
	var users []db.User
	if err := s.db.Find(&users).Error; err != nil {
		return nil, err
	}
	views := make([]*UserView, len(users))
	for i := range users {
		views[i] = userToView(&users[i])
	}
	return views, nil
}
