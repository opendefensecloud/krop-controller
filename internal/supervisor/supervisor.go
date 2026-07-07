// Copyright 2026 opendefense contributors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package supervisor owns one instance-serving manager per published blueprint
// APIExport. A single multicluster manager binds exactly one APIExport (apiexport.New
// takes one endpoint slice), so serving N blueprints means N managers — started in
// goroutines and torn down by cancelling their context.
package supervisor

import (
	"context"
	"sync"
)

// StartFunc launches a blocking instance-serving manager for one APIExport; it
// returns when the passed context is cancelled (or on fatal error).
type StartFunc func(ctx context.Context, exportName string) error

// Supervisor tracks running per-export managers.
type Supervisor struct {
	start StartFunc

	mu      sync.Mutex
	running map[string]context.CancelFunc
}

// New returns a Supervisor that uses start to launch each manager.
func New(start StartFunc) *Supervisor {
	return &Supervisor{start: start, running: map[string]context.CancelFunc{}}
}

// Ensure starts a manager for exportName if not already running (idempotent).
func (s *Supervisor) Ensure(parent context.Context, exportName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.running[exportName]; ok {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	s.running[exportName] = cancel
	go func() {
		// start blocks until ctx is cancelled; ignore the returned error here
		// (the caller observes liveness via Running / logs inside start).
		_ = s.start(ctx, exportName)
	}()
}

// Running reports whether a manager is active for exportName.
func (s *Supervisor) Running(exportName string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.running[exportName]
	return ok
}

// Stop cancels and forgets the manager for exportName.
func (s *Supervisor) Stop(exportName string) {
	s.mu.Lock()
	cancel, ok := s.running[exportName]
	delete(s.running, exportName)
	s.mu.Unlock()
	if ok {
		cancel()
	}
}
