// Package sqlite is de opslagadapter voor SQLite.
package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/golang-migrate/migrate/v4"
	migratesqlite "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "modernc.org/sqlite" // pure-Go driver, geen CGO nodig

	"tradingbot/migrations"
)

// DB is een SQLite-database met de repositories van de bot.
type DB struct {
	db *sql.DB
}

// Open opent (of maakt) de database op path en voert alle migraties uit.
// Gebruik ":memory:" voor een tijdelijke database.
func Open(path string) (*DB, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("sqlite: map aanmaken: %w", err)
		}
	}

	dsn := "file:" + filepath.ToSlash(path) + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: openen: %w", err)
	}
	// Eén verbinding: geen schrijfconflicten, en een :memory:-database blijft dezelfde.
	db.SetMaxOpenConns(1)

	if err := migrateUp(db); err != nil {
		db.Close()
		return nil, err
	}
	return &DB{db: db}, nil
}

func migrateUp(db *sql.DB) error {
	source, err := iofs.New(migrations.FS, "sqlite")
	if err != nil {
		return fmt.Errorf("sqlite: migraties laden: %w", err)
	}
	driver, err := migratesqlite.WithInstance(db, &migratesqlite.Config{})
	if err != nil {
		return fmt.Errorf("sqlite: migratiedriver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", source, "sqlite", driver)
	if err != nil {
		return fmt.Errorf("sqlite: migraties: %w", err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("sqlite: migraties uitvoeren: %w", err)
	}
	return nil
}

// Close sluit de database.
func (d *DB) Close() error {
	return d.db.Close()
}
