-- 001_initial.sql
-- Example initial migration (GORM auto-migrate handles this automatically).
-- Place raw SQL migrations here if you prefer manual migration management.

CREATE TABLE IF NOT EXISTS users (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at DATETIME,
    updated_at DATETIME,
    deleted_at DATETIME,
    name       TEXT NOT NULL,
    email      TEXT NOT NULL UNIQUE
);

CREATE INDEX IF NOT EXISTS idx_users_deleted_at ON users (deleted_at);
