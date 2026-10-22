package cronjob

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"
)

func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done != nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if (s.runner == nil && s.executor == nil) || s.lockPath == "" {
		return errors.New("cron runner and lock path are required")
	}
	unlock, err := lockScheduler(s.lockPath)
	if err != nil {
		return err
	}
	if err := s.RecoverInterrupted(ctx); err != nil {
		unlock()
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel, s.done = cancel, make(chan struct{})
	go func() {
		defer close(s.done)
		defer unlock()
		var workers sync.WaitGroup
		if s.executor != nil {
			workers.Add(1)
			go func() {
				defer workers.Done()
				ticker := time.NewTicker(2 * time.Second)
				defer ticker.Stop()
				for runCtx.Err() == nil {
					if err := s.executor.Sync(runCtx); err != nil && runCtx.Err() == nil {
						log.Printf("[paxd] Sync cron sessions: %v.", err)
					}
					select {
					case <-runCtx.Done():
					case <-ticker.C:
					}
				}
			}()
		}
		for range 4 {
			workers.Add(1)
			go func() {
				defer workers.Done()
				s.work(runCtx)
			}()
		}
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for runCtx.Err() == nil {
			if err := s.QueueDue(runCtx); err != nil && runCtx.Err() == nil {
				log.Printf("[paxd] Queue cron jobs: %v.", err)
			}
			select {
			case <-runCtx.Done():
			case <-ticker.C:
			}
		}
		workers.Wait()
	}()
	return nil
}

func (s *Service) work(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		ran, err := s.ExecuteNext(ctx)
		if err != nil && ctx.Err() == nil {
			log.Printf("[paxd] Execute cron job: %v.", err)
		}
		if ran && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
}

func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done == nil {
		return false
	}
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}
