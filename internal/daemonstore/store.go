package daemonstore

import (
	"context"
	"errors"
	"time"

	"github.com/pax-beehive/paxd/internal/control"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
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
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		return nil, err
	}
	return New(db, opts...), nil
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
	return s.db.WithContext(ctx).AutoMigrate(
		&Remote{},
		&RemoteAuth{},
		&RemoteStatus{},
		&AgentConnection{},
		&AgentConnectionStatus{},
		&ControlCommand{},
		&HarnessInventory{},
		&LocalSession{},
		&LocalSessionElement{},
		&Setting{},
	)
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
