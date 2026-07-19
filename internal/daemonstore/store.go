package daemonstore

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/pax-beehive/paxd/internal/control"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var (
	ErrNotFound  = control.ErrNotFound
	ErrDuplicate = control.ErrDuplicate
)

type Store struct {
	db  *gorm.DB
	now func() time.Time
}

type Option func(*Store)

func WithClock(now func() time.Time) Option {
	return func(store *Store) {
		store.now = now
	}
}

func OpenSQLite(path string, opts ...Option) (*Store, error) {
	if err := ensureParentDir(path); err != nil {
		return nil, err
	}
	db, err := gorm.Open(sqlite.Open(sqliteDSN(path)), &gorm.Config{
		Logger: logger.New(log.New(os.Stdout, "\r\n", log.LstdFlags), logger.Config{
			SlowThreshold:             200 * time.Millisecond,
			LogLevel:                  logger.Warn,
			IgnoreRecordNotFoundError: true,
			Colorful:                  true,
		}),
	})
	if err != nil {
		return nil, err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	return New(db, opts...), nil
}

func sqliteDSN(path string) string {
	if path == "" || path == ":memory:" {
		return path
	}
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	return path + separator + "_journal_mode=WAL&_busy_timeout=5000"
}

func ensureParentDir(path string) error {
	if path == "" || path == ":memory:" || strings.HasPrefix(path, "file:") {
		return nil
	}
	path = sqlitePathBeforeQuery(path)
	dir := filepath.Dir(path)
	if dir == "." || dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create db dir: %w", err)
	}
	return nil
}

var sqliteURIPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)

func sqlitePathBeforeQuery(path string) string {
	if sqliteURIPattern.MatchString(path) {
		return path
	}
	if value, _, ok := strings.Cut(path, "?"); ok {
		return value
	}
	return path
}

func New(db *gorm.DB, opts ...Option) *Store {
	store := &Store{
		db:  db,
		now: time.Now,
	}
	for _, opt := range opts {
		opt(store)
	}
	return store
}

func (s *Store) DB() *gorm.DB {
	return s.db
}

func (s *Store) Migrate(ctx context.Context) error {
	if err := s.dropRemoteCloudAPIURLUniqueIndex(ctx); err != nil {
		return err
	}
	if err := s.dropRemoteIsDefaultColumn(ctx); err != nil {
		return err
	}
	if err := s.dropLegacyAgentConnectionUniqueIndexes(ctx); err != nil {
		return err
	}
	return s.db.WithContext(ctx).AutoMigrate(
		&Remote{},
		&RemoteAuth{},
		&RemoteStatus{},
		&AgentConnection{},
		&AgentConnectionStatus{},
		&ACPSlotStatus{},
		&ACPSessionRoute{},
		&ControlCommand{},
		&HarnessInventory{},
		&LocalSession{},
		&LocalSessionElement{},
		&Message{},
		&MessagePart{},
		&Setting{},
	)
}

func (s *Store) dropLegacyAgentConnectionUniqueIndexes(ctx context.Context) error {
	migrator := s.db.WithContext(ctx).Migrator()
	if !migrator.HasTable(&AgentConnection{}) {
		return nil
	}
	for _, name := range []string{
		"idx_agent_connection_remote_name",
		"idx_agent_connection_remote_cloud_agent",
	} {
		var definition struct {
			SQL string
		}
		result := s.db.WithContext(ctx).
			Raw("SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?", name).
			Scan(&definition)
		if result.Error != nil {
			return fmt.Errorf("inspect agent connection index %s: %w", name, result.Error)
		}
		if result.RowsAffected == 0 || isActiveAgentConnectionIndex(definition.SQL) {
			continue
		}
		if err := migrator.DropIndex(&AgentConnection{}, name); err != nil {
			return fmt.Errorf("drop legacy agent connection index %s: %w", name, err)
		}
	}
	return nil
}

func isActiveAgentConnectionIndex(definition string) bool {
	normalized := strings.ToLower(strings.Join(strings.Fields(definition), " "))
	return strings.Contains(normalized, " where ") &&
		strings.Contains(normalized, "deleted_at") &&
		strings.Contains(normalized, "is null")
}

func (s *Store) dropRemoteCloudAPIURLUniqueIndex(ctx context.Context) error {
	migrator := s.db.WithContext(ctx).Migrator()
	if migrator.HasIndex(&Remote{}, "idx_remote_cloud_api_url") {
		if err := migrator.DropIndex(&Remote{}, "idx_remote_cloud_api_url"); err != nil {
			return fmt.Errorf("drop remote cloud api url unique index: %w", err)
		}
	}
	return nil
}

func (s *Store) dropRemoteIsDefaultColumn(ctx context.Context) error {
	migrator := s.db.WithContext(ctx).Migrator()
	if migrator.HasTable(&Remote{}) && migrator.HasColumn(&Remote{}, "is_default") {
		if err := migrator.DropColumn(&Remote{}, "is_default"); err != nil {
			return fmt.Errorf("drop remote is_default column: %w", err)
		}
	}
	return nil
}

func (s *Store) WithTx(ctx context.Context, fn func(control.TxStore) error) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return fn(&Store{db: tx, now: s.now})
	})
}

func (s *Store) currentTime() time.Time {
	return s.now().UTC()
}

func isMissing(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound)
}

func mapGormErr(err error) error {
	if err == nil {
		return nil
	}
	if isMissing(err) {
		return ErrNotFound
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return ErrDuplicate
	}
	return err
}
