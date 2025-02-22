// Copyright (c) 2022 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package dbutil

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"
)

type Dialect int

const (
	DialectUnknown Dialect = iota
	Postgres
	SQLite
)

func (dialect Dialect) String() string {
	switch dialect {
	case Postgres:
		return "postgres"
	case SQLite:
		return "sqlite3"
	default:
		return ""
	}
}

func ParseDialect(engine string) (Dialect, error) {
	switch strings.ToLower(engine) {
	case "postgres", "postgresql", "pgx":
		return Postgres, nil
	case "sqlite3", "sqlite", "litestream", "sqlite3-fk-wal":
		return SQLite, nil
	default:
		return DialectUnknown, fmt.Errorf("unknown dialect '%s'", engine)
	}
}

type Rows interface {
	Close() error
	ColumnTypes() ([]*sql.ColumnType, error)
	Columns() ([]string, error)
	Err() error
	Next() bool
	NextResultSet() bool
	Scan(...any) error
}

type Scannable interface {
	Scan(...interface{}) error
}

// Expected implementations of Scannable
var (
	_ Scannable = (*sql.Row)(nil)
	_ Scannable = (Rows)(nil)
)

type UnderlyingContextExecable interface {
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row
}

type ContextExecable interface {
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...interface{}) (Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row
}

type UnderlyingExecable interface {
	UnderlyingContextExecable
	Exec(query string, args ...interface{}) (sql.Result, error)
	Query(query string, args ...interface{}) (*sql.Rows, error)
	QueryRow(query string, args ...interface{}) *sql.Row
}

type Execable interface {
	ContextExecable
	Exec(query string, args ...interface{}) (sql.Result, error)
	Query(query string, args ...interface{}) (Rows, error)
	QueryRow(query string, args ...interface{}) *sql.Row
}

type Transaction interface {
	Execable
	Commit() error
	Rollback() error
}

// Expected implementations of Execable
var (
	_ UnderlyingExecable        = (*sql.Tx)(nil)
	_ UnderlyingExecable        = (*sql.DB)(nil)
	_ Execable                  = (*LoggingExecable)(nil)
	_ Transaction               = (*LoggingTxn)(nil)
	_ UnderlyingContextExecable = (*sql.Conn)(nil)
)

type Database struct {
	loggingDB
	RawDB        *sql.DB
	Owner        string
	VersionTable string
	Log          DatabaseLogger
	Dialect      Dialect
	UpgradeTable UpgradeTable

	IgnoreForeignTables       bool
	IgnoreUnsupportedDatabase bool
}

var positionalParamPattern = regexp.MustCompile(`\$(\d+)`)

func (db *Database) mutateQuery(query string) string {
	switch db.Dialect {
	case SQLite:
		return positionalParamPattern.ReplaceAllString(query, "?$1")
	default:
		return query
	}
}

func (db *Database) Child(versionTable string, upgradeTable UpgradeTable, log DatabaseLogger) *Database {
	if log == nil {
		log = db.Log
	}
	return &Database{
		RawDB:        db.RawDB,
		loggingDB:    db.loggingDB,
		Owner:        "",
		VersionTable: versionTable,
		UpgradeTable: upgradeTable,
		Log:          log,
		Dialect:      db.Dialect,

		IgnoreForeignTables:       true,
		IgnoreUnsupportedDatabase: db.IgnoreUnsupportedDatabase,
	}
}

func NewWithDB(db *sql.DB, rawDialect string) (*Database, error) {
	dialect, err := ParseDialect(rawDialect)
	if err != nil {
		return nil, err
	}
	wrappedDB := &Database{
		RawDB:   db,
		Dialect: dialect,
		Log:     NoopLogger,

		IgnoreForeignTables: true,
		VersionTable:        "version",
	}
	wrappedDB.loggingDB.UnderlyingExecable = db
	wrappedDB.loggingDB.db = wrappedDB
	return wrappedDB, nil
}

func NewWithDialect(uri, rawDialect string) (*Database, error) {
	db, err := sql.Open(rawDialect, uri)
	if err != nil {
		return nil, err
	}
	return NewWithDB(db, rawDialect)
}

type Config struct {
	Type string `yaml:"type"`
	URI  string `yaml:"uri"`

	MaxOpenConns int `yaml:"max_open_conns"`
	MaxIdleConns int `yaml:"max_idle_conns"`

	ConnMaxIdleTime string `yaml:"conn_max_idle_time"`
	ConnMaxLifetime string `yaml:"conn_max_lifetime"`
}

func (db *Database) Configure(cfg Config) error {
	db.RawDB.SetMaxOpenConns(cfg.MaxOpenConns)
	db.RawDB.SetMaxIdleConns(cfg.MaxIdleConns)
	if len(cfg.ConnMaxIdleTime) > 0 {
		maxIdleTimeDuration, err := time.ParseDuration(cfg.ConnMaxIdleTime)
		if err != nil {
			return fmt.Errorf("failed to parse max_conn_idle_time: %w", err)
		}
		db.RawDB.SetConnMaxIdleTime(maxIdleTimeDuration)
	}
	if len(cfg.ConnMaxLifetime) > 0 {
		maxLifetimeDuration, err := time.ParseDuration(cfg.ConnMaxLifetime)
		if err != nil {
			return fmt.Errorf("failed to parse max_conn_idle_time: %w", err)
		}
		db.RawDB.SetConnMaxLifetime(maxLifetimeDuration)
	}
	return nil
}

func NewFromConfig(owner string, cfg Config, logger DatabaseLogger) (*Database, error) {
	dialect, err := ParseDialect(cfg.Type)
	if err != nil {
		return nil, err
	}
	conn, err := sql.Open(cfg.Type, cfg.URI)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = NoopLogger
	}
	wrappedDB := &Database{
		RawDB: conn,

		Owner:   owner,
		Dialect: dialect,

		Log: logger,

		IgnoreForeignTables: true,
		VersionTable:        "version",
	}
	err = wrappedDB.Configure(cfg)
	if err != nil {
		return nil, err
	}
	wrappedDB.loggingDB.UnderlyingExecable = conn
	wrappedDB.loggingDB.db = wrappedDB
	return wrappedDB, nil
}
