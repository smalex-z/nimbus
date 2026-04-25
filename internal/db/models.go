package db

import (
	"gorm.io/gorm"
)

// User is an example GORM model demonstrating soft-delete and timestamps.
type User struct {
	gorm.Model
	Name  string `gorm:"not null" json:"name"`
	Email string `gorm:"uniqueIndex;not null" json:"email"`
}
