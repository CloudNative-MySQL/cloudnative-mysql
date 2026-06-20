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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mysqlv1alpha1 "github.com/CloudNative-MySQL/cloudnative-mysql/api/v1alpha1"
	"github.com/CloudNative-MySQL/cloudnative-mysql/pkg/management/mysql/groupreplication"
	"github.com/CloudNative-MySQL/cloudnative-mysql/pkg/management/mysql/webserver"
)

const testGRPrimary = "demo-1"
const testGRSecondary = "demo-2"

// grObserved is a GR member status reporting ONLINE PRIMARY with the given uuid.
func grObservedPrimary(instance, uuid string) *webserver.Status {
	st := &webserver.Status{
		InstanceName: instance,
		IsReady:      true,
		Role:         webserver.RolePrimary,
		GroupReplication: &webserver.GroupReplicationMemberStatus{
			MemberID: uuid,
			State:    groupreplication.MemberStateOnline,
			Role:     groupreplication.MemberRolePrimary,
			ViewID:   "view-1",
		},
	}
	for _, name := range []string{testGRPrimary, testGRSecondary} {
		role := groupreplication.MemberRoleSecondary
		if name == instance {
			role = groupreplication.MemberRolePrimary
		}
		st.GroupReplication.Members = append(st.GroupReplication.Members, webserver.GroupReplicationMember{
			MemberID: uuid + "-" + name,
			Host:     name + ".default.svc",
			Port:     3306,
			State:    groupreplication.MemberStateOnline,
			Role:     role,
		})
	}
	return st
}

// grObservedSecondary is a GR member status reporting ONLINE SECONDARY.
func grObservedSecondary(instance, uuid, primaryUUID string) *webserver.Status {
	st := &webserver.Status{
		InstanceName: instance,
		IsReady:      true,
		Role:         webserver.RoleReplica,
		GroupReplication: &webserver.GroupReplicationMemberStatus{
			MemberID: uuid,
			State:    groupreplication.MemberStateOnline,
			Role:     groupreplication.MemberRoleSecondary,
			ViewID:   "view-1",
		},
	}
	for _, name := range []string{testGRPrimary, testGRSecondary} {
		if name == instance {
			st.GroupReplication.Members = append(st.GroupReplication.Members, webserver.GroupReplicationMember{
				MemberID: uuid,
				Host:     name + ".default.svc",
				Port:     3306,
				State:    groupreplication.MemberStateOnline,
				Role:     groupreplication.MemberRoleSecondary,
			})
			continue
		}
		st.GroupReplication.Members = append(st.GroupReplication.Members, webserver.GroupReplicationMember{
			MemberID: primaryUUID,
			Host:     name + ".default.svc",
			Port:     3306,
			State:    groupreplication.MemberStateOnline,
			Role:     groupreplication.MemberRolePrimary,
		})
	}
	return st
}

// grSwitchoverCluster returns a GR cluster with currentPrimary set to the primary.
func grSwitchoverCluster(target string) *mysqlv1alpha1.Cluster {
	cluster := baseCluster()
	cluster.Spec.Instances = 2
	cluster.Spec.Replication = &mysqlv1alpha1.ReplicationConfiguration{
		Mode: mysqlv1alpha1.ReplicationModeGroupReplication,
	}
	cluster.Spec.MaxSwitchoverDelay = 3600
	cluster.Status.CurrentPrimary = testGRPrimary
	cluster.Status.TargetPrimary = target
	cluster.Status.GroupReplication = &mysqlv1alpha1.GroupReplicationStatus{
		GroupName:    "group-uuid",
		Bootstrapped: true,
	}
	return cluster
}

func TestReconcileGRSwitchoverInvokesSetAsPrimary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testScheme(t)
	cluster := grSwitchoverCluster(testGRSecondary)
	pods := []*corev1.Pod{
		readyPod(cluster, testGRPrimary, rolePrimary),
		readyPod(cluster, testGRSecondary, roleReplica),
	}
	control := &recordingControlClient{}
	reconciler := &ClusterReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(&mysqlv1alpha1.Cluster{}).
			WithObjects(cluster, pods[0], pods[1]).
			Build(),
		Scheme:        scheme,
		ControlClient: control,
	}
	observed := observedCluster{
		Plan:           testPlan(),
		PrimaryName:    testGRPrimary,
		InstanceNames:  []string{testGRPrimary, testGRSecondary},
		ReadyInstances: 2,
		StatusByInstance: map[string]*webserver.Status{
			testGRPrimary:   grObservedPrimary(testGRPrimary, "uuid-1"),
			testGRSecondary: grObservedSecondary(testGRSecondary, "uuid-2", "uuid-1"),
		},
	}

	switched, err := reconciler.reconcileSwitchover(ctx, cluster, observed.Plan, observed)
	if err != nil {
		t.Fatal(err)
	}
	if !switched {
		t.Fatal("expected switchover to be handled")
	}
	if control.setAsPrimary == nil {
		t.Fatal("expected set_as_primary to be invoked")
	}
	// It should invoke the UDF from an ONLINE member (prefer current primary).
	var caller string
	for c := range control.setAsPrimary {
		caller = c
	}
	if caller != testGRPrimary {
		t.Fatalf("set_as_primary caller = %q, want %q", caller, testGRPrimary)
	}
	if got := control.setAsPrimary[caller]; got != "uuid-2" {
		t.Fatalf("set_as_primary memberUUID = %q, want uuid-2", got)
	}
}

func TestReconcileGRSwitchoverAbortsOnTimeout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testScheme(t)
	cluster := grSwitchoverCluster(testGRSecondary)
	cluster.Status.TargetPrimaryTimestamp = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	pods := []*corev1.Pod{
		readyPod(cluster, testGRPrimary, rolePrimary),
		readyPod(cluster, testGRSecondary, roleReplica),
	}
	control := &recordingControlClient{}
	reconciler := &ClusterReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(&mysqlv1alpha1.Cluster{}).
			WithObjects(cluster, pods[0], pods[1]).
			Build(),
		Scheme:        scheme,
		ControlClient: control,
	}
	observed := observedCluster{
		Plan:           testPlan(),
		PrimaryName:    testGRPrimary,
		InstanceNames:  []string{testGRPrimary, testGRSecondary},
		ReadyInstances: 2,
		StatusByInstance: map[string]*webserver.Status{
			testGRPrimary:   grObservedPrimary(testGRPrimary, "uuid-1"),
			testGRSecondary: grObservedSecondary(testGRSecondary, "uuid-2", "uuid-1"),
		},
	}

	switched, err := reconciler.reconcileSwitchover(ctx, cluster, observed.Plan, observed)
	if err != nil {
		t.Fatal(err)
	}
	if !switched {
		t.Fatal("aborted switchover should be reported as handled")
	}
	if len(control.setAsPrimary) != 0 {
		t.Fatal("timed-out switchover must not invoke set_as_primary")
	}
	got := &mysqlv1alpha1.Cluster{}
	if err := reconciler.Get(ctx, namespacedName(cluster), got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != phaseBlocked {
		t.Fatalf("phase = %q, want %q", got.Status.Phase, phaseBlocked)
	}
}

func TestReconcileGRSwitchoverBlocksNonOnlineTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testScheme(t)
	cluster := grSwitchoverCluster(testGRSecondary)
	// Make the target show RECOVERING instead of ONLINE: validation must block.
	targetStatus := grObservedSecondary(testGRSecondary, "uuid-2", "uuid-1")
	targetStatus.GroupReplication.State = groupreplication.MemberStateRecovering
	pods := []*corev1.Pod{
		readyPod(cluster, testGRPrimary, rolePrimary),
	}
	reconciler := &ClusterReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(&mysqlv1alpha1.Cluster{}).
			WithObjects(cluster, pods[0]).
			Build(),
		Scheme:        scheme,
		ControlClient: &recordingControlClient{},
	}
	observed := observedCluster{
		Plan:           testPlan(),
		PrimaryName:    testGRPrimary,
		InstanceNames:  []string{testGRPrimary, testGRSecondary},
		ReadyInstances: 2,
		StatusByInstance: map[string]*webserver.Status{
			testGRPrimary:   grObservedPrimary(testGRPrimary, "uuid-1"),
			testGRSecondary: targetStatus,
		},
	}

	switched, err := reconciler.reconcileSwitchover(ctx, cluster, observed.Plan, observed)
	if err != nil {
		t.Fatal(err)
	}
	if !switched {
		t.Fatal("expected blocked switchover to be handled")
	}
	got := &mysqlv1alpha1.Cluster{}
	if err := reconciler.Get(ctx, namespacedName(cluster), got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != phaseBlocked {
		t.Fatalf("phase = %q, want %q", got.Status.Phase, phaseBlocked)
	}
}

func namespacedName(cluster *mysqlv1alpha1.Cluster) types.NamespacedName {
	return types.NamespacedName{Namespace: cluster.Namespace, Name: cluster.Name}
}
