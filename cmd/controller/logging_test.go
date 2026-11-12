// Copyright BWI GmbH and krop-controller contributors
// SPDX-License-Identifier: Apache-2.0

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
