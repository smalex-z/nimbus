package service

import (
	"nimbus/internal/db"
)

// ExampleService provides business logic for the example resource.
type ExampleService struct {
	db *db.DB
}

// NewExampleService creates a new ExampleService.
func NewExampleService(database *db.DB) *ExampleService {
	return &ExampleService{db: database}
}

// ListUsers returns all non-deleted users.
func (s *ExampleService) ListUsers() ([]db.User, error) {
	var users []db.User
	if err := s.db.Find(&users).Error; err != nil {
		return nil, err
	}
	return users, nil
}

// CreateUser creates a new user.
func (s *ExampleService) CreateUser(name, email string) (*db.User, error) {
	user := &db.User{Name: name, Email: email}
	if err := s.db.Create(user).Error; err != nil {
		return nil, err
	}
	return user, nil
}

// DeleteUser soft-deletes a user by ID.
func (s *ExampleService) DeleteUser(id uint) error {
	return s.db.Delete(&db.User{}, id).Error
}
