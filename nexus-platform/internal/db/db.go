package db

import (
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"nexus/platform/internal/config"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// InitDB opens the GORM connection and configures the underlying *sql.DB connection pool
// (RM-20). Without pool limits MySQL returns error 1040 ("Too many connections") under
// concurrent load; the defaults (25 open / 10 idle / 5 min lifetime) keep both pools
// (GORM + sqlx) well under MySQL's default max_connections=151.
func InitDB(cfg *config.Config) (*gorm.DB, error) {
	db, err := gorm.Open(mysql.Open(cfg.DBDSN), &gorm.Config{Logger: Logger(os.Stdout)})
	if err != nil {
		return nil, fmt.Errorf("db.InitDB: open: %w", err)
	}

	// Configure the underlying *sql.DB pool. gorm.DB.DB() never errors when the
	// driver was opened successfully, so the error is only checked for hygiene.
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("db.InitDB: get sql.DB: %w", err)
	}
	sqlDB.SetMaxOpenConns(cfg.DBMaxOpenConns)
	sqlDB.SetMaxIdleConns(cfg.DBMaxIdleConns)
	sqlDB.SetConnMaxLifetime(cfg.DBConnMaxLifetime)

	return db, nil
}

// Logger is the production GORM logger: gorm's logger.Default (Warn — failed and >200ms
// statements) plus ParameterizedQueries, so a logged statement keeps its `?` placeholders. Without
// it a duplicate-key INSERT printed every bound value to stdout: sealed envelopes and master-key
// ids (sys_named_credentials, MFA), password hashes, customer data.
func Logger(w io.Writer) logger.Interface {
	return logger.New(log.New(w, "\r\n", log.LstdFlags), logger.Config{
		SlowThreshold:        200 * time.Millisecond,
		LogLevel:             logger.Warn,
		Colorful:             true,
		ParameterizedQueries: true,
	})
}
