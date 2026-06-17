/*
Copyright 2026 The CloudNative MySQL Authors.

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mysqlv1alpha1 "github.com/CloudNative-MySQL/cloudnative-mysql/api/v1alpha1"
	"github.com/CloudNative-MySQL/cloudnative-mysql/pkg/management/mysql/webserver"
)

// partitionedControlClient simulates an instance the operator cannot reach (e.g.
// behind a NetworkPolicy): Status returns an error for the named instances and
// behaves like the recording client otherwise.
type partitionedControlClient struct {
	*recordingControlClient
	unreachable map[string]bool
}

func (c *partitionedControlClient) Status(
	ctx context.Context,
	cluster *mysqlv1alpha1.Cluster,
	instanceName string,
) (*webserver.Status, error) {
	if c.unreachable[instanceName] {
		return nil, errors.New("unreachable")
	}
	return c.recordingControlClient.Status(ctx, cluster, instanceName)
}

func TestClusterEstablished(t *testing.T) {
	t.Parallel()
	// Establishment is the sticky EstablishedAt marker, not the live phase: a
	// cluster that was once ready stays established even after its phase is
	// re-stamped back to Provisioning by an intermediate reconcile step.
	notEstablished := &mysqlv1alpha1.Cluster{}
	notEstablished.Status.Phase = phaseReady // phase alone must not count
	if clusterEstablished(notEstablished) {
		t.Error("clusterEstablished with no EstablishedAt = true, want false")
	}
	established := &mysqlv1alpha1.Cluster{}
	established.Status.Phase = phaseProvisioning // phase says provisioning...
	established.Status.EstablishedAt = &metav1.Time{Time: time.Now()}
	if !clusterEstablished(established) {
		t.Error("clusterEstablished with EstablishedAt set = false, want true")
	}
}

func TestEstablishedPhase(t *testing.T) {
	t.Parallel()
	tests := map[string]bool{
		"":                false,
		phasePending:      false,
		phaseProvisioning: false,
		phaseReady:        true,
		phaseDegraded:     true,
		phaseSwitchover:   true,
		phaseFailingOver:  true,
		phaseBlocked:      true,
	}
	for phase, want := range tests {
		if got := establishedPhase(phase); got != want {
			t.Errorf("establishedPhase(%q) = %t, want %t", phase, got, want)
		}
	}
}

func TestUnreadyInstanceNames(t *testing.T) {
	t.Parallel()
	observed := observedCluster{
		InstanceNames: []string{testPrimary, testReplica2, testReplica3},
		StatusByInstance: map[string]*webserver.Status{
			testPrimary:  {InstanceName: testPrimary, IsReady: true},
			testReplica2: {InstanceName: testReplica2, IsReady: false}, // reachable, not ready
			// testReplica3 missing entirely: unreachable
		},
	}
	got := unreadyInstanceNames(observed)
	want := []string{testReplica2, testReplica3}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("unreadyInstanceNames = %v, want %v", got, want)
	}
}

// observePartitionedReplica builds a two-instance cluster whose primary is ready
// and whose replica Pod is Ready to Kubernetes but unreachable to the operator,
// then observes it with the cluster carrying the given previously-persisted phase.
func observePartitionedReplica(t *testing.T, previousPhase string) observedCluster {
	t.Helper()
	ctx := context.Background()
	cluster := baseCluster()
	cluster.Spec.Instances = 2
	cluster.Status.CurrentPrimary = testPrimary
	cluster.Status.Phase = previousPhase
	// Mirror what patchStatus would have persisted: an operational previous phase
	// means the cluster was established at least once.
	if establishedPhase(previousPhase) {
		cluster.Status.EstablishedAt = &metav1.Time{Time: time.Now()}
	}
	scheme := testScheme(t)

	// Both Pods are Ready from Kubernetes' point of view (a NetworkPolicy does
	// not block kubelet probes), but the operator cannot reach the replica.
	primaryPod := readyPod(cluster, testPrimary, rolePrimary)
	replicaPod := readyPod(cluster, testReplica2, roleReplica)
	control := &partitionedControlClient{
		recordingControlClient: &recordingControlClient{
			statuses: map[string]*webserver.Status{
				testPrimary: {
					InstanceName: testPrimary,
					Role:         webserver.RolePrimary,
					IsReady:      true,
					GTIDExecuted: testGTID,
				},
			},
		},
		unreachable: map[string]bool{testReplica2: true},
	}
	reconciler := &ClusterReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(&mysqlv1alpha1.Cluster{}).
			WithObjects(cluster, primaryPod, replicaPod).
			Build(),
		Scheme:        scheme,
		ControlClient: control,
	}

	plan := testPlan()
	plan.Instances = 2
	observed, err := reconciler.observe(ctx, cluster, plan)
	if err != nil {
		t.Fatal(err)
	}
	if observed.ReadyInstances != 1 {
		t.Fatalf("readyInstances = %d, want 1", observed.ReadyInstances)
	}
	return observed
}

func TestObserveEstablishedClusterDegradesWhenInstanceUnreachable(t *testing.T) {
	t.Parallel()
	observed := observePartitionedReplica(t, phaseReady)
	if observed.Phase != phaseDegraded {
		t.Fatalf("phase = %q, want %q", observed.Phase, phaseDegraded)
	}
	if !strings.Contains(observed.PhaseReason, testReplica2) {
		t.Fatalf("phaseReason = %q, want it to name the unreachable instance %q", observed.PhaseReason, testReplica2)
	}
}

func TestObserveEstablishedClusterDegradesOnTotalOutage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cluster := baseCluster()
	cluster.Spec.Instances = 1
	cluster.Status.CurrentPrimary = testPrimary
	cluster.Status.Phase = phaseReady
	cluster.Status.EstablishedAt = &metav1.Time{Time: time.Now()}
	scheme := testScheme(t)

	// The sole instance's Pod still exists but the operator cannot reach it.
	primaryPod := readyPod(cluster, testPrimary, rolePrimary)
	control := &partitionedControlClient{
		recordingControlClient: &recordingControlClient{statuses: map[string]*webserver.Status{}},
		unreachable:            map[string]bool{testPrimary: true},
	}
	reconciler := &ClusterReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(&mysqlv1alpha1.Cluster{}).
			WithObjects(cluster, primaryPod).
			Build(),
		Scheme:        scheme,
		ControlClient: control,
	}

	plan := testPlan()
	plan.Instances = 1
	observed, err := reconciler.observe(ctx, cluster, plan)
	if err != nil {
		t.Fatal(err)
	}
	if observed.ReadyInstances != 0 {
		t.Fatalf("readyInstances = %d, want 0", observed.ReadyInstances)
	}
	// A fully-down established cluster must read Degraded, not "Pending: waiting
	// for the primary instance" (which implies it is still being provisioned).
	if observed.Phase != phaseDegraded {
		t.Fatalf("phase = %q, want %q", observed.Phase, phaseDegraded)
	}
	if !strings.Contains(observed.PhaseReason, testPrimary) {
		t.Fatalf("phaseReason = %q, want it to name the unreachable instance %q", observed.PhaseReason, testPrimary)
	}
}

func TestObserveBootstrappingClusterStaysPending(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cluster := baseCluster()
	cluster.Spec.Instances = 1
	scheme := testScheme(t)

	// Initial bootstrap: the primary Pod is not Ready yet and the cluster has no
	// prior phase. This must stay Pending, not Degraded.
	primaryPod := readyPod(cluster, testPrimary, rolePrimary)
	primaryPod.Status.Conditions = nil
	reconciler := &ClusterReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(&mysqlv1alpha1.Cluster{}).
			WithObjects(cluster, primaryPod).
			Build(),
		Scheme: scheme,
		ControlClient: &partitionedControlClient{
			recordingControlClient: &recordingControlClient{statuses: map[string]*webserver.Status{}},
			unreachable:            map[string]bool{testPrimary: true},
		},
	}

	plan := testPlan()
	plan.Instances = 1
	observed, err := reconciler.observe(ctx, cluster, plan)
	if err != nil {
		t.Fatal(err)
	}
	if observed.Phase != phasePending {
		t.Fatalf("phase = %q, want %q", observed.Phase, phasePending)
	}
}

func TestObserveProvisioningClusterStaysProvisioning(t *testing.T) {
	t.Parallel()
	// A cluster still completing initial provisioning must not be reported as
	// Degraded just because not every instance is ready yet.
	observed := observePartitionedReplica(t, phaseProvisioning)
	if observed.Phase != phaseProvisioning {
		t.Fatalf("phase = %q, want %q", observed.Phase, phaseProvisioning)
	}
}

// crashLoopPod builds an instance Pod whose container is stuck in
// CrashLoopBackOff past the restart threshold: it never became Ready.
func crashLoopPod(cluster *mysqlv1alpha1.Cluster, name, role string) *corev1.Pod {
	pod := readyPod(cluster, name, role)
	pod.Status.Conditions = nil
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:         "mysql",
		RestartCount: crashLoopRestartThreshold,
		State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
		},
	}}
	return pod
}

func TestObserveCrashloopingInstanceDegradesBeforeEstablished(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cluster := baseCluster()
	cluster.Spec.Instances = 2
	cluster.Status.CurrentPrimary = testPrimary
	// Never established: the cluster is still in its initial provisioning phase
	// and EstablishedAt is unset. A crashlooping instance must still surface as
	// Degraded rather than sitting silently in Provisioning.
	cluster.Status.Phase = phaseProvisioning
	scheme := testScheme(t)

	primaryPod := readyPod(cluster, testPrimary, rolePrimary)
	replicaPod := crashLoopPod(cluster, testReplica2, roleReplica)
	control := &recordingControlClient{
		statuses: map[string]*webserver.Status{
			testPrimary: {
				InstanceName: testPrimary,
				Role:         webserver.RolePrimary,
				IsReady:      true,
				GTIDExecuted: testGTID,
			},
		},
	}
	reconciler := &ClusterReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(&mysqlv1alpha1.Cluster{}).
			WithObjects(cluster, primaryPod, replicaPod).
			Build(),
		Scheme:        scheme,
		ControlClient: control,
	}

	plan := testPlan()
	plan.Instances = 2
	observed, err := reconciler.observe(ctx, cluster, plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(observed.FailedInstances) != 1 || observed.FailedInstances[0] != testReplica2 {
		t.Fatalf("failedInstances = %v, want [%s]", observed.FailedInstances, testReplica2)
	}
	if observed.Phase != phaseDegraded {
		t.Fatalf("phase = %q, want %q", observed.Phase, phaseDegraded)
	}
	if !strings.Contains(observed.PhaseReason, testReplica2) {
		t.Fatalf("phaseReason = %q, want it to name the failing instance %q", observed.PhaseReason, testReplica2)
	}
}

func TestPatchStatusEstablishedAtIsSticky(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cluster := baseCluster()
	cluster.Spec.Instances = 1
	scheme := testScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&mysqlv1alpha1.Cluster{}).
		WithObjects(cluster).
		Build()
	reconciler := &ClusterReconciler{Client: c, Scheme: scheme}
	plan := testPlan()
	key := types.NamespacedName{Namespace: cluster.Namespace, Name: cluster.Name}

	// 1) The cluster becomes fully ready: EstablishedAt is recorded.
	if err := reconciler.patchStatus(ctx, cluster, observedCluster{
		Plan:           plan,
		InstanceNames:  []string{testPrimary},
		Phase:          phaseReady,
		Ready:          true,
		ReadyInstances: 1,
	}); err != nil {
		t.Fatal(err)
	}
	got := &mysqlv1alpha1.Cluster{}
	if err := c.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.EstablishedAt == nil {
		t.Fatal("EstablishedAt not set after the cluster first became Ready")
	}
	first := got.Status.EstablishedAt.DeepCopy()

	// 2) An intermediate step re-stamps the phase to Provisioning. EstablishedAt
	// must survive, so the cluster is still considered established.
	if err := reconciler.patchStatus(ctx, got, observedCluster{
		Plan:           plan,
		InstanceNames:  []string{testPrimary},
		Phase:          phaseProvisioning,
		Ready:          false,
		ReadyInstances: 0,
	}); err != nil {
		t.Fatal(err)
	}
	got2 := &mysqlv1alpha1.Cluster{}
	if err := c.Get(ctx, key, got2); err != nil {
		t.Fatal(err)
	}
	if got2.Status.EstablishedAt == nil {
		t.Fatal("EstablishedAt was erased by a later Provisioning patch")
	}
	if !got2.Status.EstablishedAt.Equal(first) {
		t.Fatalf("EstablishedAt changed: was %v, now %v", first, got2.Status.EstablishedAt)
	}
	if !clusterEstablished(got2) {
		t.Fatal("cluster no longer reports established after a Provisioning re-stamp")
	}
}
