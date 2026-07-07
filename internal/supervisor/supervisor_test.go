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
