package db

import (
	"gorm.io/gorm"
)

// User represents an account in Nimbus.
type User struct {
	gorm.Model
	Name         string `gorm:"not null" json:"name"`
	Email        string `gorm:"uniqueIndex;not null" json:"email"`
	PasswordHash string `gorm:"default:''" json:"-"`
}
