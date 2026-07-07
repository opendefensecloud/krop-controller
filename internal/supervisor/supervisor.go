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
	"errors"
	"sync"

	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// StartFunc launches a blocking instance-serving manager for one APIExport; it
// returns when the passed context is cancelled (or on fatal error).
type StartFunc func(ctx context.Context, exportName string) error

// entry is one running per-export manager. It is tracked by pointer identity so
// forget can distinguish generations across a fast stop/restart (context.CancelFunc
// values are not comparable, so a token pointer is used instead).
type entry struct {
	cancel context.CancelFunc
}

// Supervisor tracks running per-export managers.
type Supervisor struct {
	start StartFunc

	mu      sync.Mutex
	running map[string]*entry
}

// New returns a Supervisor that uses start to launch each manager.
func New(start StartFunc) *Supervisor {
	return &Supervisor{start: start, running: map[string]*entry{}}
}

// Ensure starts a manager for exportName if not already running (idempotent).
func (s *Supervisor) Ensure(parent context.Context, exportName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.running[exportName]; ok {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	e := &entry{cancel: cancel}
	s.running[exportName] = e
	go func() {
		// Clear our entry on return so a crashed/exited manager can be
		// re-Ensured. forget checks pointer identity, so it never removes a
		// newer entry created by a stop/restart while this start was returning.
		defer s.forget(exportName, e)
		// start blocks until ctx is cancelled. A clean shutdown returns nil (or the
		// context's cancellation error); any OTHER terminal error means the per-export
		// manager fell over — surface it at the supervisor boundary so a persistently
		// failing manager is visible (forget still clears the entry so the next Ensure
		// self-heals by restarting it).
		if err := s.start(ctx, exportName); err != nil &&
			!errors.Is(err, context.Canceled) && ctx.Err() == nil {
			logf.Log.WithName("supervisor").Error(err, "instance-serving manager terminated with error", "export", exportName)
		}
	}()
}

// forget deletes the running entry for exportName only if it is still e, so a
// slow-exiting manager cannot clobber a newer generation.
func (s *Supervisor) forget(exportName string, e *entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running[exportName] == e {
		delete(s.running, exportName)
	}
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
	e, ok := s.running[exportName]
	delete(s.running, exportName)
	s.mu.Unlock()
	if ok {
		e.cancel()
	}
}
