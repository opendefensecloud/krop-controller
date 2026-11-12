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

package e2e

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
)

// The deployed controller runs with --zap-log-level=2 (see deployChart), so its log
// holds every tier. zap writes each line's tier into "level" (ADR 0013).
const (
	levelInfo  = "info"      // default
	levelError = "error"     // default
	levelV1    = "debug"     // --zap-log-level=debug
	levelV2    = "Level(-2)" // --zap-log-level=2
)

// logLine is one JSON line of the deployed controller's log.
type logLine struct {
	Raw    string
	Level  string
	Msg    string
	Fields map[string]any
}

// str returns a field as a string ("" when absent or not a string).
func (l logLine) str(key string) string {
	s, _ := l.Fields[key].(string)

	return s
}

// strs returns a list field as strings.
func (l logLine) strs(key string) []string {
	raw, _ := l.Fields[key].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, _ := v.(string)
		out = append(out, s)
	}

	return out
}

// defaultLevel reports whether the line shows at the default --zap-log-level=info.
func (l logLine) defaultLevel() bool {
	return l.Level == levelInfo || l.Level == levelError
}

// controllerLog returns the deployed controller's log lines, all of them when since
// is zero. Lines that are not JSON objects are returned in other.
func controllerLog(since time.Time) (lines []logLine, other []string) {
	GinkgoHelper()
	args := []string{"-n", kropNamespace, "logs", "-l", "app.kubernetes.io/name=krop-controller", "--tail=-1"}
	if !since.IsZero() {
		args = append(args, "--since-time="+since.UTC().Format(time.RFC3339))
	}
	for raw := range strings.SplitSeq(kindctl(args...), "\n") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		fields := map[string]any{}
		if !strings.HasPrefix(raw, "{") || json.Unmarshal([]byte(raw), &fields) != nil {
			other = append(other, raw)

			continue
		}
		l := logLine{Raw: raw, Fields: fields}
		l.Level, _ = fields["level"].(string)
		l.Msg, _ = fields["msg"].(string)
		lines = append(lines, l)
	}

	return lines, other
}

// findLog returns the lines with message msg whose fields include every key/value
// in match.
func findLog(lines []logLine, msg string, match map[string]string) []logLine {
	var out []logLine
	for _, l := range lines {
		if l.Msg != msg {
			continue
		}
		ok := true
		for k, v := range match {
			if l.str(k) != v {
				ok = false

				break
			}
		}
		if ok {
			out = append(out, l)
		}
	}

	return out
}

// waitForLog waits until the controller has logged a line with message msg whose
// fields include match, at the given level, and returns the first such line.
func waitForLog(timeout time.Duration, level, msg string, match map[string]string) logLine {
	GinkgoHelper()
	var found logLine
	waitFor(timeout, fmt.Sprintf("controller log %q %v", msg, match), func() error {
		lines, _ := controllerLog(time.Time{})
		got := findLog(lines, msg, match)
		if len(got) == 0 {
			return fmt.Errorf("no %q line with %v yet", msg, match)
		}
		if got[0].Level != level {
			return fmt.Errorf("%q logged at %q, want %q: %s", msg, got[0].Level, level, got[0].Raw)
		}
		found = got[0]

		return nil
	})

	return found
}

// duplicateKey returns the first top-level key a JSON object line repeats, or "".
// encoding/json keeps the last value of a repeated key, so a duplicate (e.g. a
// field added both by the logger's context and by the call) is only visible in the
// raw line.
func duplicateKey(raw string) string {
	dec := json.NewDecoder(strings.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return ""
	}
	var seen []string
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return ""
		}
		key, _ := t.(string)
		if slices.Contains(seen, key) {
			return key
		}
		seen = append(seen, key)
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return ""
		}
	}

	return ""
}
