/*
Copyright 2026 The CloudNative MySQL Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	mysqlv1alpha1 "github.com/CloudNative-MySQL/cloudnative-mysql/api/v1alpha1"
	"github.com/CloudNative-MySQL/cloudnative-mysql/pkg/management/mysql/groupreplication"
)

// groupReplicationTopology implements topologyReconciler for MySQL Group
// Replication. The group owns HA decisions; the operator only declares desired
// membership, observes replication_group_members, and mirrors the group's
// decisions into Kubernetes status and routing.
type groupReplicationTopology struct{}

func (groupReplicationTopology) Name() string { return "groupReplication" }

func (groupReplicationTopology) DonorAvailable(observed observedCluster) bool {
	return groupHasOnlineDonor(observed)
}

func (groupReplicationTopology) NeedsPrimaryLease() bool { return false }

func (groupReplicationTopology) IsSemiSyncRelevant() bool { return false }

// ReconcileFailover is intentionally disabled for Group Replication. The group
// auto-elects a new primary when the old one fails; the operator must never
// inject an async failover decision. The actual failover is surfaced in
// ObserveTopology, which mirrors the group's elected PRIMARY into currentPrimary.
func (groupReplicationTopology) ReconcileFailover(
	_ context.Context,
	_ *ClusterReconciler,
	_ *mysqlv1alpha1.Cluster,
	_ clusterPlan,
	_ observedCluster,
) (bool, ctrl.Result, error) {
	return false, ctrl.Result{}, nil
}

// ReconcileSwitchover performs a planned primary change via the group's
// group_replication_set_as_primary UDF. The operator validates the target is an
// ONLINE SECONDARY, invokes the UDF on any ONLINE member, and then lets the
// terminal status write mirror the new PRIMARY into currentPrimary once the group
// reports the handover complete. It is bounded by spec.maxSwitchoverDelay exactly
// like the async path.
//
// Completion is detected from the group's own elected primary (observed.PrimaryName),
// never from status.currentPrimary: under GR the operator is the sole writer of
// currentPrimary and only mirrors it after the fact, so the stale status value
// would never advance while this step keeps taking over the reconcile. Comparing
// against the live group view lets the step yield (handled=false) the moment the
// group has promoted the target, so the terminal patchStatus persists it and
// repoints -rw.
func (groupReplicationTopology) ReconcileSwitchover(
	ctx context.Context,
	r *ClusterReconciler,
	cluster *mysqlv1alpha1.Cluster,
	plan clusterPlan,
	observed observedCluster,
) (bool, error) {
	target := cluster.Status.TargetPrimary
	if target == "" {
		return false, nil
	}
	// The group's elected primary, not the possibly-stale status.currentPrimary, is
	// the source of truth for whether the switchover still has work to do.
	groupPrimary := observed.PrimaryName
	if target == groupPrimary {
		// The group has the target as PRIMARY (handover done, or it never moved):
		// nothing to drive. Yield so the terminal status write mirrors it into
		// currentPrimary and repoints routing, and stop the switchover stopwatch.
		if err := r.clearSwitchoverDeadline(ctx, cluster); err != nil {
			return false, err
		}
		return false, nil
	}
	if groupPrimary == "" {
		// Bootstrap: no elected primary observed yet. Observation will set
		// currentPrimary once the first member is ONLINE PRIMARY; until then
		// targetPrimary is just the bootstrap designee.
		return false, nil
	}

	if err := validateGRSwitchoverTarget(observed, target); err != nil {
		return true, r.patchGRStatus(ctx, cluster, plan, observed, phaseBlocked, err.Error(), false, false)
	}

	startedAt, err := r.ensureSwitchoverStarted(ctx, cluster)
	if err != nil {
		return false, err
	}
	maxDelay := time.Duration(cluster.Spec.MaxSwitchoverDelay) * time.Second
	if maxDelay > 0 && time.Since(startedAt) > maxDelay {
		// Give up without promoting anyone back: the group never left a consistent
		// state, so restoring the target to the group's actual primary (and never
		// re-issuing set_as_primary toward it) is the whole revert.
		return r.abortSwitchover(ctx, cluster, groupPrimary, target)
	}

	targetStatus := observed.StatusByInstance[target]
	memberUUID := targetStatus.GroupReplication.MemberID
	if memberUUID == "" {
		return true, fmt.Errorf("target %s has no group member UUID", target)
	}

	caller := pickOnlineMember(observed, groupPrimary, target)
	if caller == "" {
		return true, fmt.Errorf("no ONLINE group member available to invoke set_as_primary")
	}

	logf.FromContext(ctx).Info("Requesting group switchover",
		"from", groupPrimary, "to", target, "memberUUID", memberUUID, "caller", caller)
	if err := r.instanceControlClient().SetAsPrimary(ctx, cluster, caller, memberUUID); err != nil {
		return true, fmt.Errorf("set_as_primary via %s: %w", caller, err)
	}

	return true, r.patchGRStatus(ctx, cluster, plan, observed, phaseSwitchover,
		fmt.Sprintf("Switching group primary to %s", target), false, true)
}

func (groupReplicationTopology) ReconcileAvailability(
	_ context.Context,
	_ *ClusterReconciler,
	_ *mysqlv1alpha1.Cluster,
	_ observedCluster,
) error {
	// No async-style availability adjustments under GR; quorum and consistency are
	// managed by the group itself. Later phases add quorum-loss surfacing and
	// guarded recovery, but those are handled in the main reconcile path.
	return nil
}

func (groupReplicationTopology) ObserveTopology(
	_ context.Context,
	_ *mysqlv1alpha1.Cluster,
	observed *observedCluster,
) {
	grStatus, primary := observeGroupReplication(*observed)
	observed.GroupReplication = grStatus
	if primary != "" {
		observed.PrimaryName = primary
	}
}

// validateGRSwitchoverTarget checks that the requested target is an ONLINE
// SECONDARY group member.
func validateGRSwitchoverTarget(observed observedCluster, target string) error {
	status, ok := observed.StatusByInstance[target]
	if !ok {
		return fmt.Errorf("target primary %s is not reporting status", target)
	}
	gr := status.GroupReplication
	if gr == nil {
		return fmt.Errorf("target primary %s has no group replication status", target)
	}
	if gr.State != groupreplication.MemberStateOnline {
		return fmt.Errorf("target primary %s is %s, want ONLINE", target, gr.State)
	}
	if gr.Role != groupreplication.MemberRoleSecondary {
		return fmt.Errorf("target primary %s has role %s, want SECONDARY", target, gr.Role)
	}
	return nil
}

// pickOnlineMember returns an ONLINE member to invoke set_as_primary on. It
// prefers the current primary, then the target, then any other ONLINE member.
func pickOnlineMember(observed observedCluster, current, target string) string {
	for _, name := range []string{current, target} {
		if isGroupMemberOnline(observed, name) {
			return name
		}
	}
	for _, name := range observed.InstanceNames {
		if isGroupMemberOnline(observed, name) {
			return name
		}
	}
	return ""
}

func isGroupMemberOnline(observed observedCluster, name string) bool {
	status, ok := observed.StatusByInstance[name]
	if !ok {
		return false
	}
	gr := status.GroupReplication
	return gr != nil && gr.State == groupreplication.MemberStateOnline
}
