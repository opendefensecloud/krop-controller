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

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kropengine "go.opendefense.cloud/krop-controller/internal/engine"
)

// Sweeper garbage-collects orphaned provider-target children.
//
// The orphan problem (idea.md §11): provider-target children live in the
// provider workspace, tracked only by the instance-uid label — owner references
// cannot reach cross-workspace. The instance finalizer normally deletes them on
// instance delete. BUT if a consumer UNBINDS the APIExport mid-life, the virtual
// workspace disengages that consumer's logical cluster: the instance reconciler
// stops running for it, the finalizer NEVER fires, and the provider children
// orphan forever. The consumer instance is no longer observable from the
// provider side, so recovery cannot reconcile against it.
//
// The mechanism: on every complete apply pass the reconciler refreshes a
// provider-workspace LIVENESS RECORD (a ConfigMap in RecordNamespace, labeled
// krop.opendefense.cloud/liveness=true) carrying the instance-uid label, a
// lastReconciled RFC3339 timestamp, and the provider-child GVKs to delete. A
// live instance keeps its record fresh (the reconciler requeues periodically);
// an unbound instance stops, so its record goes stale. The Sweeper lists records,
// and for any whose lastReconciled is older than StaleAfter it deletes that
// instance's provider children (by instance-uid label, for each recorded GVK)
// and then the record itself.
//
// TIMING INVARIANT: StaleAfter MUST comfortably exceed the reconciler's refresh
// interval (the caller's requeueInterval) with margin, else a still-live instance
// whose refresh merely lagged could be swept. See the SweepInterval/StaleAfter
// consts in cmd/controller.
type Sweeper struct {
	// ProviderClient reads liveness records and deletes provider children in the
	// provider workspace.
	ProviderClient client.Client
	// RecordNamespace holds the liveness records. Defaults to "default".
	RecordNamespace string
	// StaleAfter is the age past which a record's instance is deemed orphaned.
	StaleAfter time.Duration
	// SweepInterval is the tick period of the Start runnable loop.
	SweepInterval time.Duration
	// Clock is injectable for tests; defaults to time.Now.
	Clock func() time.Time
}

// now returns the current time via the injected clock, or time.Now.
func (s *Sweeper) now() time.Time {
	if s.Clock != nil {
		return s.Clock()
	}

	return time.Now()
}

// recordNamespace returns the configured record namespace, or "default".
func (s *Sweeper) recordNamespace() string {
	if s.RecordNamespace != "" {
		return s.RecordNamespace
	}

	return defaultRecordNamespace
}

// Start ticks every SweepInterval, calling Sweep, until ctx is cancelled. It
// satisfies sigs.k8s.io/controller-runtime manager.Runnable so it can be added
// with mgr.Add(s). A sweep error is logged, not returned, so one bad pass does
// not tear down the manager.
func (s *Sweeper) Start(ctx context.Context) error {
	l := log.FromContext(ctx).WithName("orphan-sweeper")
	interval := s.SweepInterval
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	l.Info("starting orphan sweeper", "interval", interval, "staleAfter", s.StaleAfter, "namespace", s.recordNamespace())
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := s.Sweep(ctx); err != nil {
				l.Error(err, "sweep pass failed")
			}
		}
	}
}

// Sweep lists liveness records and reclaims those that have gone stale: for each
// stale record it deletes the instance's provider children (by instance-uid
// label, for each recorded GVK) and then the record itself. Records that fail to
// parse are logged and skipped, never crashing the pass.
func (s *Sweeper) Sweep(ctx context.Context) error {
	l := log.FromContext(ctx).WithName("orphan-sweeper")

	records := &unstructured.UnstructuredList{}
	records.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMapList"})
	if err := s.ProviderClient.List(ctx, records,
		client.InNamespace(s.recordNamespace()),
		client.MatchingLabels{kropengine.LabelLiveness: "true"}); err != nil {
		return fmt.Errorf("listing liveness records: %w", err)
	}

	now := s.now()
	for i := range records.Items {
		rec := &records.Items[i]
		instanceUID := rec.GetLabels()[kropengine.LabelInstanceUID]

		lastRaw, _, _ := unstructured.NestedString(rec.Object, "data", "lastReconciled")
		last, err := time.Parse(time.RFC3339, lastRaw)
		if err != nil {
			// Garbage / missing timestamp: skip rather than delete blindly.
			l.Info("skipping unparseable liveness record", "record", rec.GetName(), "lastReconciled", lastRaw)
			continue
		}
		if now.Sub(last) <= s.StaleAfter {
			continue // still fresh: the instance is (or recently was) live.
		}

		l.Info("orphaned instance: sweeping provider children",
			"record", rec.GetName(), "instanceUID", instanceUID, "age", now.Sub(last).String())

		if err := s.sweepChildren(ctx, rec, instanceUID); err != nil {
			// Log and continue: leave the record so the next pass retries.
			l.Error(err, "sweeping children failed; leaving record for retry", "record", rec.GetName())
			continue
		}

		if err := s.ProviderClient.Delete(ctx, rec); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("deleting liveness record %s: %w", rec.GetName(), err)
		}
	}

	return nil
}

// sweepChildren deletes every provider child of the orphaned instance: for each
// GVK recorded in the liveness record's data, list children by the instance-uid
// label and delete them (ignoring not-found).
func (s *Sweeper) sweepChildren(ctx context.Context, rec *unstructured.Unstructured, instanceUID string) error {
	l := log.FromContext(ctx).WithName("orphan-sweeper")
	if instanceUID == "" {
		return fmt.Errorf("liveness record %s missing %s label", rec.GetName(), kropengine.LabelInstanceUID)
	}

	gvkRaw, _, _ := unstructured.NestedString(rec.Object, "data", "providerChildGVKs")
	sel := client.MatchingLabels{kropengine.LabelInstanceUID: instanceUID}
	for token := range strings.SplitSeq(gvkRaw, ",") {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		gvk, err := parseGVKToken(token)
		if err != nil {
			l.Info("skipping unparseable child GVK", "record", rec.GetName(), "token", token)
			continue
		}

		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(schema.GroupVersionKind{Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind + "List"})
		if err := s.ProviderClient.List(ctx, list, sel); err != nil {
			return fmt.Errorf("listing %s children: %w", gvk.Kind, err)
		}
		for j := range list.Items {
			child := &list.Items[j]
			if err := s.ProviderClient.Delete(ctx, child); client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("deleting %s %s: %w", gvk.Kind, child.GetName(), err)
			}
			l.Info("swept orphaned provider child", "gvk", token, "name", child.GetName())
		}
	}

	return nil
}

// parseGVKToken parses a "group/version/Kind" token (core group has an empty
// first segment, e.g. "/v1/ConfigMap") back into a GVK.
func parseGVKToken(token string) (schema.GroupVersionKind, error) {
	parts := strings.SplitN(token, "/", 3)
	if len(parts) != 3 || parts[1] == "" || parts[2] == "" {
		return schema.GroupVersionKind{}, fmt.Errorf("malformed GVK token %q", token)
	}

	return schema.GroupVersionKind{Group: parts[0], Version: parts[1], Kind: parts[2]}, nil
}
