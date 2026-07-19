package db

import (
	"context"
	"embed"
	"fmt"
	"log"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// InitDB connects to Postgres, runs migrations, and returns the connection pool.
func InitDB(databaseURL string) (*pgxpool.Pool, error) {
	if err := runMigrations(databaseURL); err != nil {
		return nil, fmt.Errorf("failed to run migrations: %w", err)
	}

	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse database URL: %w", err)
	}

	pool, err := pgxpool.NewWithConfig(context.Background(), poolConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	if err := pool.Ping(context.Background()); err != nil {
		return nil, fmt.Errorf("database is not responding: %w", err)
	}

	log.Println("Database connection established and migrations applied successfully!")
	return pool, nil
}

// runMigrations points to our embedded SQL files and executes them.
func runMigrations(databaseURL string) error {
	// 1. Load the embedded files
	d, err := iofs.New(migrationFS, "migrations")
	if err != nil {
		return err
	}

	// 2. Tell the migrate tool to use the embedded files instead of the hard drive
	m, err := migrate.NewWithSourceInstance("iofs", d, databaseURL)
	if err != nil {
		return err
	}

	// 3. Execute the "Up" migrations
	err = m.Up()
	if err != nil {
		if err == migrate.ErrNoChange {
			log.Println("Migrations already up to date.")
			return nil
		}
		log.Printf("Migration failed with error: %v\n", err)
		return err
	}
	log.Println("Migrations executed successfully!")
	return nil
}
