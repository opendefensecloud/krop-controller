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

package main

import (
	"bytes"
	"errors"
	"flag"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// errorLine builds the production logger from args the way main does and returns
// the line one Error call writes.
func errorLine(t *testing.T, args ...string) string {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	opts := newZapOptions()
	opts.BindFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parsing %v: %v", args, err)
	}
	var buf bytes.Buffer
	zap.New(zap.UseFlagOptions(&opts), zap.WriteTo(&buf)).Error(errors.New("boom"), "Reconciler error")

	return buf.String()
}

// Error lines carry no Go stack trace by default: they are reconcile errors the
// message already explains, and a trace makes each one many lines long.
func TestZapOptions_NoStacktraceByDefault(t *testing.T) {
	if line := errorLine(t); strings.Contains(line, `"stacktrace"`) {
		t.Fatalf("default error line carries a stack trace: %s", line)
	}
}

// --zap-stacktrace-level still turns traces back on.
func TestZapOptions_StacktraceFlagOverrides(t *testing.T) {
	if line := errorLine(t, "--zap-stacktrace-level=error"); !strings.Contains(line, `"stacktrace"`) {
		t.Fatalf("--zap-stacktrace-level=error did not add a stack trace: %s", line)
	}
}
