package db

import (
	"time"

	"gorm.io/gorm"
)

// User represents an account in Nimbus.
type User struct {
	gorm.Model
	Name         string `gorm:"not null" json:"name"`
	Email        string `gorm:"uniqueIndex;not null" json:"email"`
	PasswordHash string `gorm:"default:''" json:"-"`
	IsAdmin      bool   `gorm:"default:false" json:"is_admin"`
}

// Session ties a browser cookie to a user for a limited duration.
type Session struct {
	ID        string    `gorm:"primaryKey" json:"id"`
	UserID    uint      `gorm:"not null;index" json:"user_id"`
	ExpiresAt time.Time `gorm:"not null" json:"expires_at"`
	CreatedAt time.Time
}

// OAuthSettings stores OAuth provider credentials. Only a single row (ID=1) is used.
type OAuthSettings struct {
	ID                 uint   `gorm:"primaryKey"`
	GitHubClientID     string `gorm:"default:''"`
	GitHubClientSecret string `gorm:"default:''"`
	GoogleClientID     string `gorm:"default:''"`
	GoogleClientSecret string `gorm:"default:''"`
}

// VM is the canonical record for a provisioned virtual machine.
type VM struct {
	gorm.Model
	VMID       int    `gorm:"column:vmid;uniqueIndex;not null"        json:"vmid"`
	Hostname   string `gorm:"column:hostname;uniqueIndex;not null"    json:"hostname"`
	IP         string `gorm:"column:ip;index;not null"                json:"ip"`
	Node       string `gorm:"column:node;not null"                    json:"node"`
	Tier       string `gorm:"column:tier;not null"                    json:"tier"`
	OSTemplate string `gorm:"column:os_template;not null"             json:"os_template"`
	Username   string `gorm:"column:username"                         json:"username"`
	Status     string `gorm:"column:status;index;not null"            json:"status"`
	OwnerID    *uint  `gorm:"column:owner_id;index"                   json:"owner_id,omitempty"`
	ErrorMsg   string `gorm:"column:error_msg"                        json:"error_msg,omitempty"`
}
