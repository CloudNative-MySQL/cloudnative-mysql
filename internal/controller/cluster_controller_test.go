/*
Copyright 2026 The CNMySQL Authors.

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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mysqlv1alpha1 "github.com/yyewolf/cnmysql/api/v1alpha1"
	"github.com/yyewolf/cnmysql/pkg/management/mysql/replication"
	"github.com/yyewolf/cnmysql/pkg/management/mysql/webserver"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := mysqlv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func baseCluster() *mysqlv1alpha1.Cluster {
	cluster := &mysqlv1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "demo",
			Namespace: "default",
		},
		Spec: mysqlv1alpha1.ClusterSpec{
			Instances: 1,
			Storage:   mysqlv1alpha1.StorageConfiguration{Size: "1Gi"},
			Bootstrap: &mysqlv1alpha1.BootstrapConfiguration{
				InitDB: &mysqlv1alpha1.BootstrapInitDB{
					Database: "app",
					Owner:    "app",
				},
			},
		},
	}
	cluster.SetDefaults()
	return cluster
}

type readyStatusClient struct{}

func (readyStatusClient) Status(context.Context, *mysqlv1alpha1.Cluster, string) (*webserver.Status, error) {
	return &webserver.Status{
		InstanceName:  "demo-1",
		Role:          webserver.RolePrimary,
		Version:       defaultMySQL80ServerVersion,
		IsReady:       true,
		GTIDExecuted:  "uuid:1-10",
		UptimeSeconds: int64(time.Minute.Seconds()),
	}, nil
}

func (readyStatusClient) Promote(context.Context, *mysqlv1alpha1.Cluster, string) error {
	return nil
}

func (readyStatusClient) Demote(context.Context, *mysqlv1alpha1.Cluster, string) error {
	return nil
}

func (readyStatusClient) ConfigureReplica(context.Context, *mysqlv1alpha1.Cluster, string, replication.SourceOptions) error {
	return nil
}

type recordingControlClient struct {
	statuses   map[string]*webserver.Status
	demoted    []string
	promoted   []string
	configured map[string]replication.SourceOptions
}

func (c *recordingControlClient) Status(_ context.Context, _ *mysqlv1alpha1.Cluster, instanceName string) (*webserver.Status, error) {
	return c.statuses[instanceName], nil
}

func (c *recordingControlClient) Promote(_ context.Context, _ *mysqlv1alpha1.Cluster, instanceName string) error {
	c.promoted = append(c.promoted, instanceName)
	return nil
}

func (c *recordingControlClient) Demote(_ context.Context, _ *mysqlv1alpha1.Cluster, instanceName string) error {
	c.demoted = append(c.demoted, instanceName)
	return nil
}

func (c *recordingControlClient) ConfigureReplica(_ context.Context, _ *mysqlv1alpha1.Cluster, instanceName string, source replication.SourceOptions) error {
	if c.configured == nil {
		c.configured = map[string]replication.SourceOptions{}
	}
	c.configured[instanceName] = source
	return nil
}

func TestBuildPlanDefaultsToLocalInstanceImage(t *testing.T) {
	t.Parallel()
	cluster := baseCluster()
	reconciler := &ClusterReconciler{
		Client: fake.NewClientBuilder().WithScheme(testScheme(t)).Build(),
		Scheme: testScheme(t),
	}

	plan, err := reconciler.buildPlan(context.Background(), cluster)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Image != defaultInstanceImage {
		t.Fatalf("image = %q, want %q", plan.Image, defaultInstanceImage)
	}
	if plan.ServerVersion != defaultMySQL80ServerVersion {
		t.Fatalf("server version = %q, want %q", plan.ServerVersion, defaultMySQL80ServerVersion)
	}
}

func TestBuildPlanResolvesNamespacedImageCatalog(t *testing.T) {
	t.Parallel()
	cluster := baseCluster()
	cluster.Spec.ImageCatalogRef = &mysqlv1alpha1.ImageCatalogRef{
		TypedLocalObjectReference: corev1.TypedLocalObjectReference{
			Name: "images",
			Kind: "ImageCatalog",
		},
		Major: 8,
	}
	scheme := testScheme(t)
	reconciler := &ClusterReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(&mysqlv1alpha1.ImageCatalog{
			ObjectMeta: metav1.ObjectMeta{Name: "images", Namespace: "default"},
			Spec: mysqlv1alpha1.ImageCatalogSpec{Images: []mysqlv1alpha1.CatalogImage{
				{Major: 8, Image: "registry.example/cnmysql:8.0"},
			}},
		}).Build(),
		Scheme: scheme,
	}

	plan, err := reconciler.buildPlan(context.Background(), cluster)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Image != "registry.example/cnmysql:8.0" {
		t.Fatalf("image = %q", plan.Image)
	}
	if plan.ServerVersion != defaultMySQL80ServerVersion {
		t.Fatalf("server version = %q", plan.ServerVersion)
	}
}

func TestResolveServerVersionFromImageTag(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"cnmysql-instance:5.6":       defaultMySQL56ServerVersion,
		"cnmysql-instance:8.0":       defaultMySQL80ServerVersion,
		"cnmysql-instance:8.4":       defaultMySQL84ServerVersion,
		"cnmysql-instance:9.x":       defaultMySQL9xServerVersion,
		"registry/cnmysql:8.0.46-37": "8.0.46-37",
	}

	for image, want := range tests {
		got, err := resolveServerVersion(image)
		if err != nil {
			t.Fatalf("resolveServerVersion(%q): %v", image, err)
		}
		if got != want {
			t.Fatalf("resolveServerVersion(%q) = %q, want %q", image, got, want)
		}
	}
}

func TestEnsurePasswordSecretDoesNotOverwriteExistingSecret(t *testing.T) {
	t.Parallel()
	cluster := baseCluster()
	scheme := testScheme(t)
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-root", Namespace: "default"},
		Data:       map[string][]byte{"password": []byte("keep")},
	}
	reconciler := &ClusterReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, existing).Build(),
		Scheme: scheme,
	}

	if err := reconciler.ensurePasswordSecret(context.Background(), cluster, "demo-root", map[string]string{"username": "root"}); err != nil {
		t.Fatal(err)
	}
	got := &corev1.Secret{}
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "demo-root"}, got); err != nil {
		t.Fatal(err)
	}
	if string(got.Data["password"]) != "keep" {
		t.Fatalf("password was overwritten: %q", got.Data["password"])
	}
}

func TestPodSpecUsesInitContainerAndCertManagerSecrets(t *testing.T) {
	t.Parallel()
	cluster := baseCluster()
	plan := testPlan()

	spec := (&ClusterReconciler{}).podSpec(cluster, plan, plan.instanceFor(cluster, 1))
	if len(spec.InitContainers) != 1 {
		t.Fatalf("init containers = %d", len(spec.InitContainers))
	}
	if got := strings.Join(spec.InitContainers[0].Args, " "); !strings.Contains(got, "instance initdb") {
		t.Fatalf("init container args = %q", got)
	}
	if got := strings.Join(spec.Containers[0].Args, " "); !strings.Contains(got, "instance run") {
		t.Fatalf("main container args = %q", got)
	}
	if got := strings.Join(spec.Containers[0].Args, " "); !strings.Contains(got, "--role=primary") {
		t.Fatalf("primary main container should declare primary role: %q", got)
	}
	if spec.Containers[0].ReadinessProbe.TCPSocket == nil {
		t.Fatalf("readiness probe must be TCP because the HTTP API requires mTLS")
	}
	volumes := map[string]string{}
	for _, volume := range spec.Volumes {
		if volume.Secret != nil {
			volumes[volume.Name] = volume.Secret.SecretName
		}
	}
	if volumes["server-tls"] != "demo-1-server-tls" {
		t.Fatalf("server tls volume = %q", volumes["server-tls"])
	}
	if volumes["client-ca"] != "demo-ca" {
		t.Fatalf("client ca volume = %q", volumes["client-ca"])
	}
}

func TestPodSpecReplicaUsesJoin(t *testing.T) {
	t.Parallel()
	cluster := baseCluster()
	plan := testPlan()
	plan.Instances = 3

	spec := (&ClusterReconciler{}).podSpec(cluster, plan, plan.instanceFor(cluster, 2))
	got := strings.Join(spec.InitContainers[0].Args, " ")
	if !strings.Contains(got, "instance join") {
		t.Fatalf("replica init container should join: %q", got)
	}
	if !strings.Contains(got, "--source-manager-url=https://demo-1.default.svc:8080/cluster/backup") {
		t.Fatalf("replica should clone from the primary manager: %q", got)
	}
	if !strings.Contains(got, "--source-host=demo-1.default.svc") {
		t.Fatalf("replica should replicate from the primary: %q", got)
	}
	got = strings.Join(spec.Containers[0].Args, " ")
	if !strings.Contains(got, "--role=replica") {
		t.Fatalf("replica main container should declare replica role: %q", got)
	}
	if !strings.Contains(got, "--source-host=demo-1.default.svc") {
		t.Fatalf("replica main container should be able to repair replication source: %q", got)
	}
	for _, container := range []corev1.Container{spec.InitContainers[0], spec.Containers[0]} {
		for _, env := range container.Env {
			if env.Name == "MYSQL_REPLICATION_PASSWORD" {
				t.Fatalf("%s must use mTLS-only replication auth, found MYSQL_REPLICATION_PASSWORD env", container.Name)
			}
		}
	}
}

func TestEnsurePodRecreatesWhenTemplateHashChanges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cluster := baseCluster()
	plan := testPlan()
	inst := plan.instanceFor(cluster, 1)
	stalePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      inst.Name,
			Namespace: cluster.Namespace,
			Annotations: map[string]string{
				podTemplateHashAnnotation: "stale",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:  "mysql",
			Image: "old",
		}}},
	}
	scheme := testScheme(t)
	reconciler := &ClusterReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, stalePod).Build(),
		Scheme: scheme,
	}

	if err := reconciler.ensurePod(ctx, cluster, plan, inst); err != nil {
		t.Fatal(err)
	}
	got := &corev1.Pod{}
	err := reconciler.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: inst.Name}, got)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("stale Pod get error = %v, want not found", err)
	}

	if err := reconciler.ensurePod(ctx, cluster, plan, inst); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: inst.Name}, got); err != nil {
		t.Fatal(err)
	}
	if got.Annotations[podTemplateHashAnnotation] == "" {
		t.Fatalf("pod template hash annotation is empty")
	}
	if got.Annotations[configHashAnnotation] == "" {
		t.Fatalf("config hash annotation is empty")
	}
	if got.Spec.Containers[0].Image != plan.Image {
		t.Fatalf("container image = %q, want %q", got.Spec.Containers[0].Image, plan.Image)
	}
}

func TestEnsurePodDoesNotRecreateForPrimaryRoleChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cluster := baseCluster()
	cluster.Spec.Instances = 2
	plan := testPlan()
	plan.Instances = 2
	inst := plan.instanceFor(cluster, 1)
	labels := labelsFor(cluster, inst.Name, roleOf(inst))
	spec := (&ClusterReconciler{}).podSpec(cluster, plan, inst)
	annotations, err := podAnnotations(cluster, plan, inst, labels, spec)
	if err != nil {
		t.Fatal(err)
	}
	existingPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        inst.Name,
			Namespace:   cluster.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: spec,
	}
	scheme := testScheme(t)
	reconciler := &ClusterReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, existingPod).Build(),
		Scheme: scheme,
	}

	plan.PrimaryName = "demo-2"
	inst = plan.instanceFor(cluster, 1)
	if err := reconciler.ensurePod(ctx, cluster, plan, inst); err != nil {
		t.Fatal(err)
	}

	got := &corev1.Pod{}
	if err := reconciler.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: "demo-1"}, got); err != nil {
		t.Fatal(err)
	}
	if got.DeletionTimestamp != nil {
		t.Fatal("pod should not be deleted when only the primary role changes")
	}
	if got.Labels[roleLabel] != roleReplica {
		t.Fatalf("role label = %q, want replica", got.Labels[roleLabel])
	}
}

func TestUnsupportedReasonNamesDeferredMilestones(t *testing.T) {
	t.Parallel()
	// Replicas are now supported.
	cluster := baseCluster()
	cluster.Spec.Instances = 3
	if got := unsupportedReason(cluster); got != "" {
		t.Fatalf("3-instance cluster should be supported, got %q", got)
	}

	cluster = baseCluster()
	cluster.Spec.Bootstrap.InitDB = nil
	cluster.Spec.Bootstrap.Recovery = &mysqlv1alpha1.BootstrapRecovery{}
	if got := unsupportedReason(cluster); !strings.Contains(got, "M6") {
		t.Fatalf("recovery unsupported reason = %q", got)
	}
}

func TestReconcileBlocksUnsupportedClusterShape(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cluster := baseCluster()
	cluster.Spec.Replica = &mysqlv1alpha1.ReplicaClusterConfiguration{Source: "external"}
	scheme := testScheme(t)
	recorder := record.NewFakeRecorder(10)
	reconciler := &ClusterReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(&mysqlv1alpha1.Cluster{}).
			WithObjects(cluster).
			Build(),
		Scheme:   scheme,
		Recorder: recorder,
	}

	result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: cluster.Namespace,
		Name:      cluster.Name,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != 0 {
		t.Fatalf("requeue after = %s, want 0", result.RequeueAfter)
	}

	got := &mysqlv1alpha1.Cluster{}
	if err := reconciler.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: cluster.Name}, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != phaseBlocked {
		t.Fatalf("phase = %q, want %q", got.Status.Phase, phaseBlocked)
	}
	if !strings.Contains(got.Status.PhaseReason, "replica") {
		t.Fatalf("phase reason = %q, want replica-cluster block", got.Status.PhaseReason)
	}
	ready := apimeta.FindStatusCondition(got.Status.Conditions, conditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse {
		t.Fatalf("ready condition = %#v, want False", ready)
	}

	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, "Warning") || !strings.Contains(event, phaseBlocked) {
			t.Fatalf("blocked event = %q, want Warning %s", event, phaseBlocked)
		}
	default:
		t.Fatalf("expected a Warning %s event", phaseBlocked)
	}
}

func TestReconcileBootstrapsSingleInstanceToReady(t *testing.T) {
	t.Parallel()
	const primaryName = "demo-1"
	ctx := context.Background()
	cluster := baseCluster()
	scheme := testScheme(t)
	recorder := record.NewFakeRecorder(10)
	reconciler := &ClusterReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(&mysqlv1alpha1.Cluster{}).
			WithObjects(cluster).
			Build(),
		Scheme:        scheme,
		Recorder:      recorder,
		ControlClient: readyStatusClient{},
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: cluster.Namespace,
		Name:      cluster.Name,
	}}

	result, err := reconciler.Reconcile(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter == 0 {
		t.Fatalf("first reconcile should requeue while waiting for cert-manager secrets")
	}
	assertOwnedObject(t, ctx, reconciler, &corev1.Secret{}, "demo-root")
	assertOwnedObject(t, ctx, reconciler, &corev1.Secret{}, "demo-app")
	assertOwnedObject(t, ctx, reconciler, &corev1.Secret{}, "demo-replication")
	assertOwnedObject(t, ctx, reconciler, &corev1.Secret{}, "demo-backup")
	assertOwnedObject(t, ctx, reconciler, &corev1.Secret{}, "demo-control")
	assertOwnedObject(t, ctx, reconciler, &corev1.Service{}, "demo-rw")
	assertOwnedObject(t, ctx, reconciler, &corev1.Service{}, "demo-ro")
	assertOwnedObject(t, ctx, reconciler, &corev1.Service{}, "demo-r")
	assertOwnedUnstructuredResource(t, ctx, reconciler, issuerGVK.Kind, issuerGVK, "demo-selfsigned")
	assertOwnedUnstructuredResource(t, ctx, reconciler, certificateGVK.Kind, certificateGVK, "demo-1-server")

	for _, name := range []string{"demo-ca", "demo-1-server-tls", "demo-client-tls"} {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cluster.Namespace},
			Data: map[string][]byte{
				"ca.crt":  []byte("ca"),
				"tls.crt": []byte("cert"),
				"tls.key": []byte("key"),
			},
		}
		if err := reconciler.Create(ctx, secret); err != nil {
			t.Fatal(err)
		}
	}

	result, err = reconciler.Reconcile(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter == 0 {
		t.Fatalf("second reconcile should requeue while waiting for pod readiness")
	}
	assertOwnedObject(t, ctx, reconciler, &corev1.ConfigMap{}, "demo-1-config")
	assertOwnedObject(t, ctx, reconciler, &corev1.PersistentVolumeClaim{}, "demo-1")
	assertOwnedObject(t, ctx, reconciler, &corev1.Service{}, "demo-1")
	pod := &corev1.Pod{}
	assertOwnedObject(t, ctx, reconciler, pod, "demo-1")
	if pod.Annotations[podTemplateHashAnnotation] == "" {
		t.Fatalf("pod template hash annotation is empty")
	}
	pod.Status.Conditions = []corev1.PodCondition{{
		Type:   corev1.PodReady,
		Status: corev1.ConditionTrue,
	}}
	if err := reconciler.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}

	result, err = reconciler.Reconcile(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != readyResync {
		t.Fatalf("ready reconcile requeue after = %s, want %s", result.RequeueAfter, readyResync)
	}

	got := &mysqlv1alpha1.Cluster{}
	if err := reconciler.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: cluster.Name}, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != phaseReady {
		t.Fatalf("phase = %q, want %q", got.Status.Phase, phaseReady)
	}
	if got.Status.CurrentPrimary != primaryName {
		t.Fatalf("current primary = %q, want %s", got.Status.CurrentPrimary, primaryName)
	}
	if got.Status.ReadyInstances != 1 {
		t.Fatalf("ready instances = %d, want 1", got.Status.ReadyInstances)
	}
	if got.Status.Image != defaultInstanceImage {
		t.Fatalf("status image = %q, want %q", got.Status.Image, defaultInstanceImage)
	}
	if got.Status.GTIDExecutedByInstance[primaryName] != "uuid:1-10" {
		t.Fatalf("gtid status = %#v", got.Status.GTIDExecutedByInstance)
	}
	ready := apimeta.FindStatusCondition(got.Status.Conditions, conditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue {
		t.Fatalf("ready condition = %#v, want True", ready)
	}

	if !drainEvents(recorder.Events, phaseReady) {
		t.Fatalf("expected a %q phase-transition event", phaseReady)
	}

	// A steady-state resync with no phase change must not emit another event.
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-recorder.Events:
		t.Fatalf("unexpected event on steady-state resync: %q", event)
	default:
	}
}

func TestReconcilePrimaryChangeSwitchesToHealthyReplica(t *testing.T) {
	t.Parallel()
	const (
		oldPrimary = "demo-1"
		newPrimary = "demo-2"
		replica    = "demo-3"
	)
	ctx := context.Background()
	cluster := baseCluster()
	cluster.Spec.Instances = 3
	cluster.Status.CurrentPrimary = oldPrimary
	cluster.Status.TargetPrimary = newPrimary
	scheme := testScheme(t)
	pod1 := readyPod(cluster, oldPrimary, rolePrimary)
	pod2 := readyPod(cluster, newPrimary, roleReplica)
	pod3 := readyPod(cluster, replica, roleReplica)
	control := &recordingControlClient{}
	reconciler := &ClusterReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(&mysqlv1alpha1.Cluster{}).
			WithObjects(cluster, pod1, pod2, pod3).
			Build(),
		Scheme:        scheme,
		ControlClient: control,
	}
	plan := testPlan()
	plan.Instances = 3
	observed := observedCluster{
		Phase:          phaseReady,
		PhaseReason:    "All instances are ready",
		Ready:          true,
		Progressing:    false,
		Plan:           plan,
		PrimaryName:    oldPrimary,
		ReadyInstances: 3,
		InstanceNames:  []string{oldPrimary, newPrimary, replica},
		GTIDByInstance: map[string]string{oldPrimary: "uuid:1-10", newPrimary: "uuid:1-10", replica: "uuid:1-10"},
		StatusByInstance: map[string]*webserver.Status{
			oldPrimary: {InstanceName: oldPrimary, Role: webserver.RolePrimary, IsReady: true, GTIDExecuted: "uuid:1-10"},
			newPrimary: {
				InstanceName: newPrimary,
				Role:         webserver.RoleReplica,
				IsReady:      true,
				GTIDExecuted: "uuid:1-10",
				Replication:  &webserver.ReplicationStatus{IORunning: true, SQLRunning: true},
			},
			replica: {
				InstanceName: replica,
				Role:         webserver.RoleReplica,
				IsReady:      true,
				GTIDExecuted: "uuid:1-10",
				Replication:  &webserver.ReplicationStatus{IORunning: true, SQLRunning: true},
			},
		},
	}

	switched, err := reconciler.reconcilePrimaryChange(ctx, cluster, plan, observed)
	if err != nil {
		t.Fatal(err)
	}
	if !switched {
		t.Fatal("primary change was not reconciled")
	}
	if got := strings.Join(control.demoted, ","); got != oldPrimary {
		t.Fatalf("demoted = %q, want %s", got, oldPrimary)
	}
	if got := strings.Join(control.promoted, ","); got != newPrimary {
		t.Fatalf("promoted = %q, want %s", got, newPrimary)
	}
	source, ok := control.configured[oldPrimary]
	if !ok {
		t.Fatalf("old primary was not configured as replica: %#v", control.configured)
	}
	if source.Host != "demo-2.default.svc" || source.User != replicationUser || !source.AutoPosition || !source.SSL {
		t.Fatalf("source = %#v", source)
	}
	if _, configuredTarget := control.configured[newPrimary]; configuredTarget {
		t.Fatalf("promoted target must not be configured as its own replica")
	}

	gotCluster := &mysqlv1alpha1.Cluster{}
	if err := reconciler.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: cluster.Name}, gotCluster); err != nil {
		t.Fatal(err)
	}
	if gotCluster.Status.CurrentPrimary != newPrimary || gotCluster.Status.TargetPrimary != newPrimary {
		t.Fatalf("primary status = current %q target %q", gotCluster.Status.CurrentPrimary, gotCluster.Status.TargetPrimary)
	}
	gotPod1 := &corev1.Pod{}
	if err := reconciler.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: oldPrimary}, gotPod1); err != nil {
		t.Fatal(err)
	}
	gotPod2 := &corev1.Pod{}
	if err := reconciler.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: newPrimary}, gotPod2); err != nil {
		t.Fatal(err)
	}
	if gotPod1.Labels[roleLabel] != roleReplica || gotPod2.Labels[roleLabel] != rolePrimary {
		t.Fatalf("role labels: demo-1=%q demo-2=%q", gotPod1.Labels[roleLabel], gotPod2.Labels[roleLabel])
	}
}

func TestReconcileReplicaSourcesRepairsReplicaFollowingOldPrimary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cluster := baseCluster()
	control := &recordingControlClient{}
	observed := observedCluster{
		PrimaryName:   "demo-2",
		InstanceNames: []string{"demo-1", "demo-2", "demo-3"},
		StatusByInstance: map[string]*webserver.Status{
			"demo-1": {
				InstanceName: "demo-1",
				Role:         webserver.RoleReplica,
				IsReady:      true,
				Replication:  &webserver.ReplicationStatus{SourceHost: "demo-2.default.svc", IORunning: true, SQLRunning: true},
			},
			"demo-2": {InstanceName: "demo-2", Role: webserver.RolePrimary, IsReady: true},
			"demo-3": {
				InstanceName: "demo-3",
				Role:         webserver.RoleReplica,
				IsReady:      true,
				Replication:  &webserver.ReplicationStatus{SourceHost: "demo-1.default.svc", IORunning: true, SQLRunning: true},
			},
		},
	}
	reconciler := &ClusterReconciler{ControlClient: control}

	repaired, err := reconciler.reconcileReplicaSources(ctx, cluster, testPlan(), observed)
	if err != nil {
		t.Fatal(err)
	}
	if !repaired {
		t.Fatal("replica source repair was not reported")
	}
	if _, ok := control.configured["demo-1"]; ok {
		t.Fatalf("replica already following current primary should not be reconfigured: %#v", control.configured)
	}
	source, ok := control.configured["demo-3"]
	if !ok {
		t.Fatalf("replica following old primary was not repaired: %#v", control.configured)
	}
	if source.Host != "demo-2.default.svc" {
		t.Fatalf("source host = %q, want demo-2.default.svc", source.Host)
	}
}

// drainEvents reports whether any buffered event mentions the given phase.
func drainEvents(events <-chan string, phase string) bool {
	found := false
	for {
		select {
		case event := <-events:
			if strings.Contains(event, phase) {
				found = true
			}
		default:
			return found
		}
	}
}

func testPlan() clusterPlan {
	return clusterPlan{
		Image:             "cnmysql-instance:8.0",
		ServerVersion:     "8.0.46",
		Instances:         1,
		RootSecretName:    "demo-root",
		AppSecretName:     "demo-app",
		ReplicationSecret: "demo-replication",
		ControlSecretName: "demo-control",
		BackupSecretName:  "demo-backup",
		CASecretName:      "demo-ca",
		ClientTLSSecret:   "demo-client-tls",
		RWServiceName:     "demo-rw",
		ROServiceName:     "demo-ro",
		RServiceName:      "demo-r",
	}
}

func readyPod(cluster *mysqlv1alpha1.Cluster, name, role string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				clusterLabel:  cluster.Name,
				instanceLabel: name,
				roleLabel:     role,
			},
		},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
			Type:   corev1.PodReady,
			Status: corev1.ConditionTrue,
		}}},
	}
}

func assertOwnedObject(t *testing.T, ctx context.Context, reconciler *ClusterReconciler, obj client.Object, name string) {
	t.Helper()
	if err := reconciler.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, obj); err != nil {
		t.Fatal(err)
	}
	if len(obj.GetOwnerReferences()) != 1 || obj.GetOwnerReferences()[0].Name != "demo" {
		t.Fatalf("%T owner refs = %#v, want demo owner", obj, obj.GetOwnerReferences())
	}
}

func assertOwnedUnstructuredResource(
	t *testing.T,
	ctx context.Context,
	reconciler *ClusterReconciler,
	resourceName string,
	gvk schema.GroupVersionKind,
	name string,
) {
	t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	if err := reconciler.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, obj); err != nil {
		t.Fatalf("%s %s: %v", resourceName, name, err)
	}
	if len(obj.GetOwnerReferences()) != 1 || obj.GetOwnerReferences()[0].Name != "demo" {
		t.Fatalf("%s owner refs = %#v, want demo owner", resourceName, obj.GetOwnerReferences())
	}
}
