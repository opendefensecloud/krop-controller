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

package supervisor

import (
	"context"
	"sync"
	"testing"
)

func TestSupervisor_EnsureIsIdempotent_AndStopReleases(t *testing.T) {
	// start runs in a goroutine, so its side effects are synchronized with a
	// mutex and a barrier channel to keep the count assertions deterministic
	// (also race-clean under `go test -race`). The closure blocks on ctx.Done
	// to model a real per-export manager, making Stop's cancellation observable.
	var mu sync.Mutex
	started := 0
	startedCh := make(chan string, 8)

	s := New(func(ctx context.Context, export string) error {
		mu.Lock()
		started++
		mu.Unlock()
		startedCh <- export
		<-ctx.Done()

		return nil
	})
	count := func() int { mu.Lock(); defer mu.Unlock(); return started }

	s.Ensure(context.Background(), "export-a")
	s.Ensure(context.Background(), "export-a") // second call: already running, no new start
	<-startedCh                                // wait for the single start to run
	if got := count(); got != 1 {
		t.Fatalf("start called %d times, want 1 (idempotent)", got)
	}
	if !s.Running("export-a") {
		t.Fatal("export-a should be running")
	}

	s.Stop("export-a")
	if s.Running("export-a") {
		t.Fatal("export-a should be stopped")
	}

	s.Ensure(context.Background(), "export-a") // restart after stop
	<-startedCh
	if got := count(); got != 2 {
		t.Fatalf("start called %d, want 2 after restart", got)
	}
}

// TestSupervisor_ChangedPublishRestarts_UnchangedDoesNot models the OnPublished
// wiring in cmd/controller/main.go: on a CHANGED specHash the closure Stops the
// running manager before Ensure (so the restarted startFn re-reads the new graph),
// and on an UNCHANGED resync it only Ensures (a no-op while the manager runs). This
// is the supervisor-level proof of the 5a change-detected-restart contract.
func TestSupervisor_ChangedPublishRestarts_UnchangedDoesNot(t *testing.T) {
	var mu sync.Mutex
	started := 0
	startedCh := make(chan string, 8)
	s := New(func(ctx context.Context, export string) error {
		mu.Lock()
		started++
		mu.Unlock()
		startedCh <- export
		<-ctx.Done()

		return nil
	})
	count := func() int { mu.Lock(); defer mu.Unlock(); return started }

	// onPublished mirrors the main.go closure: always update (implicit here), Stop
	// only when changed, then Ensure.
	onPublished := func(export string, changed bool) {
		if changed {
			s.Stop(export)
		}
		s.Ensure(context.Background(), export)
	}

	// 1. First publish of a new blueprint (changed=true): nothing running, Stop is a
	//    no-op, Ensure starts the manager once.
	onPublished("export-a", true)
	<-startedCh
	if got := count(); got != 1 {
		t.Fatalf("first publish: start called %d, want 1", got)
	}

	// 2. Unchanged 5m resync (changed=false): Ensure is a no-op, no restart.
	onPublished("export-a", false)
	onPublished("export-a", false)
	if got := count(); got != 1 {
		t.Fatalf("unchanged resync must NOT restart: start called %d, want 1", got)
	}
	if !s.Running("export-a") {
		t.Fatal("export-a should still be running after unchanged resync")
	}

	// 3. Spec edit republished (changed=true): Stop+Ensure restarts the manager so
	//    it re-reads the new graph.
	onPublished("export-a", true)
	<-startedCh
	if got := count(); got != 2 {
		t.Fatalf("changed publish must restart: start called %d, want 2", got)
	}
}
