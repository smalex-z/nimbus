package db

import (
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// DB wraps a GORM database connection.
type DB struct {
	*gorm.DB
}

// New opens a SQLite database at the given path and auto-migrates all models.
func New(path string) (*DB, error) {
	gormDB, err := gorm.Open(sqlite.Open(path), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, err
	}

	// Configure connection pool.
	sqlDB, err := gormDB.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(1) // SQLite supports one writer at a time
	sqlDB.SetMaxIdleConns(1)

	if err := gormDB.Exec("PRAGMA foreign_keys = ON").Error; err != nil {
		return nil, err
	}

	// Auto-migrate all models.
	if err := gormDB.AutoMigrate(&User{}); err != nil {
		return nil, err
	}

	return &DB{gormDB}, nil
}
