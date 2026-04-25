package db

import (
	"fmt"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// DB wraps a GORM database connection.
type DB struct {
	*gorm.DB
}

// New opens a SQLite database at the given path and auto-migrates the supplied
// model types. SQLite's single-writer constraint is enforced by capping the
// connection pool at 1.
//
// Each package owning models passes its own pointer types so that there is no
// import cycle between the db layer and feature packages.
func New(path string, models ...any) (*DB, error) {
	gormDB, err := gorm.Open(sqlite.Open(path), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("open sqlite at %s: %w", path, err)
	}

	sqlDB, err := gormDB.DB()
	if err != nil {
		return nil, fmt.Errorf("get underlying sql.DB: %w", err)
	}
	sqlDB.SetMaxOpenConns(1) // SQLite supports one writer at a time
	sqlDB.SetMaxIdleConns(1)

	if err := gormDB.Exec("PRAGMA foreign_keys = ON").Error; err != nil {
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}

	if len(models) > 0 {
		if err := gormDB.AutoMigrate(models...); err != nil {
			return nil, fmt.Errorf("auto-migrate: %w", err)
		}
	}

	return &DB{gormDB}, nil
}
