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

// Package testlog captures structured log lines in tests, so assertions can
// check which lines a component emits, at which verbosity, with which fields.
// It is imported only by tests.
package testlog

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
)

// Line is one captured log line.
type Line struct {
	// Msg is the log message.
	Msg string
	// Level is the V level of an info line (0 for default); 0 for error lines.
	Level int
	// Err is the error text of an error line, empty for info lines.
	Err string
	// Values holds every other key/value pair, as decoded from JSON. A key
	// written twice keeps only its last value here; Raw shows both.
	Values map[string]any
	// Raw is the line exactly as written.
	Raw string
}

// Sink collects the lines a logger from New writes. Safe for concurrent use.
type Sink struct {
	mu    sync.Mutex
	raw   []string
	lines []Line
}

// New returns a logger that records every line up to verbosity v into the
// returned Sink.
func New(verbosity int) (logr.Logger, *Sink) {
	s := &Sink{}
	lg := funcr.NewJSON(s.write, funcr.Options{Verbosity: verbosity})

	return lg, s
}

func (s *Sink) write(obj string) {
	values := map[string]any{}
	_ = json.Unmarshal([]byte(obj), &values)
	line := Line{Values: values, Raw: obj}
	if msg, ok := values["msg"].(string); ok {
		line.Msg = msg
	}
	if lvl, ok := values["level"].(float64); ok {
		line.Level = int(lvl)
	}
	if e, ok := values["error"].(string); ok {
		line.Err = e
	}
	for _, k := range []string{"msg", "level", "error", "logger"} {
		delete(values, k)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.raw = append(s.raw, obj)
	s.lines = append(s.lines, line)
}

// Lines returns a copy of every captured line.
func (s *Sink) Lines() []Line {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]Line(nil), s.lines...)
}

// Find returns the captured lines whose message is msg.
func (s *Sink) Find(msg string) []Line {
	var out []Line
	for _, l := range s.Lines() {
		if l.Msg == msg {
			out = append(out, l)
		}
	}

	return out
}

// Raw returns every captured line as written, newline-joined, for substring checks.
func (s *Sink) Raw() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return strings.Join(s.raw, "\n")
}
