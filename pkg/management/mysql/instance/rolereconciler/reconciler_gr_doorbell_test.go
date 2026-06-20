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

package rolereconciler

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mysqlv1alpha1 "github.com/CloudNative-MySQL/cloudnative-mysql/api/v1alpha1"
	"github.com/CloudNative-MySQL/cloudnative-mysql/pkg/management/mysql/groupreplication"
	"github.com/CloudNative-MySQL/cloudnative-mysql/pkg/management/mysql/replication"
	"github.com/CloudNative-MySQL/cloudnative-mysql/pkg/management/mysql/webserver"
)

func grDoorbellCluster(
	t *testing.T,
	status *mysqlv1alpha1.ClusterStatus,
	pod *corev1.Pod,
	local *fakeLocal,
) *Reconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := mysqlv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cluster := &mysqlv1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "default"},
		Spec: mysqlv1alpha1.ClusterSpec{
			Instances: 3,
			Replication: &mysqlv1alpha1.ReplicationConfiguration{
				Mode: mysqlv1alpha1.ReplicationModeGroupReplication,
			},
		},
	}
	if status != nil {
		cluster.Status = *status
	}
	objects := []client.Object{cluster}
	if pod != nil {
		objects = append(objects, pod)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&mysqlv1alpha1.Cluster{}).
		WithObjects(objects...).
		Build()
	return &Reconciler{
		Client:         c,
		ClusterKey:     types.NamespacedName{Namespace: "default", Name: "demo"},
		InstanceName:   "demo-1",
		ServiceDomain:  "default.svc",
		SourceTemplate: replication.SourceOptions{User: "repl"},
		Local:          local,
	}
}

func TestGROnlineMemberBumpsObservedDoorbell(t *testing.T) {
	t.Parallel()
	local := &fakeLocal{status: &webserver.Status{
		Role: webserver.RolePrimary,
		GroupReplication: &webserver.GroupReplicationMemberStatus{
			MemberID:        "uuid-1",
			State:           groupreplication.MemberStateOnline,
			Role:            groupreplication.MemberRolePrimary,
			ViewID:          "view-1",
			PrimaryMemberID: "uuid-1",
			Members: []webserver.GroupReplicationMember{{
				MemberID: "uuid-1",
				Host:     "demo-1.default.svc",
				Port:     3306,
				State:    groupreplication.MemberStateOnline,
				Role:     groupreplication.MemberRolePrimary,
			}},
		},
	}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "demo-1",
			Namespace: "default",
		},
	}
	r := grDoorbellCluster(t, &mysqlv1alpha1.ClusterStatus{TargetPrimary: "demo-1"}, pod, local)
	reconcile(t, r)

	got := &corev1.Pod{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "demo-1"}, got); err != nil {
		t.Fatal(err)
	}
	val := got.Annotations[grObservedAnnotation]
	if val == "" {
		t.Fatal("expected GR observed annotation to be set")
	}
	if val != "uuid-1:view-1:ONLINE:true" {
		t.Fatalf("annotation = %q, want uuid-1:view-1:ONLINE:true", val)
	}
}

func TestGRObservedDoorbellIsIdempotent(t *testing.T) {
	t.Parallel()
	local := &fakeLocal{status: &webserver.Status{
		Role: webserver.RolePrimary,
		GroupReplication: &webserver.GroupReplicationMemberStatus{
			MemberID:        "uuid-1",
			State:           groupreplication.MemberStateOnline,
			Role:            groupreplication.MemberRolePrimary,
			ViewID:          "view-1",
			PrimaryMemberID: "uuid-1",
			Members: []webserver.GroupReplicationMember{{
				MemberID: "uuid-1",
				State:    groupreplication.MemberStateOnline,
				Role:     groupreplication.MemberRolePrimary,
			}},
		},
	}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "demo-1",
			Namespace: "default",
			Annotations: map[string]string{
				grObservedAnnotation: "uuid-1:view-1:ONLINE:true",
			},
		},
	}
	r := grDoorbellCluster(t, &mysqlv1alpha1.ClusterStatus{TargetPrimary: "demo-1"}, pod, local)
	reconcile(t, r)

	got := &corev1.Pod{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "demo-1"}, got); err != nil {
		t.Fatal(err)
	}
	val := got.Annotations[grObservedAnnotation]
	if val != "uuid-1:view-1:ONLINE:true" {
		t.Fatalf("annotation changed to %q, want idempotent", val)
	}
}

func TestGRObservedDoorbellUpdatesOnChange(t *testing.T) {
	t.Parallel()
	local := &fakeLocal{status: &webserver.Status{
		Role: webserver.RoleReplica,
		GroupReplication: &webserver.GroupReplicationMemberStatus{
			MemberID:        "uuid-1",
			State:           groupreplication.MemberStateOnline,
			Role:            groupreplication.MemberRoleSecondary,
			ViewID:          "view-2",
			PrimaryMemberID: "uuid-2",
			Members: []webserver.GroupReplicationMember{{
				MemberID: "uuid-1",
				State:    groupreplication.MemberStateOnline,
				Role:     groupreplication.MemberRoleSecondary,
			}, {
				MemberID: "uuid-2",
				State:    groupreplication.MemberStateOnline,
				Role:     groupreplication.MemberRolePrimary,
			}},
		},
	}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "demo-1",
			Namespace: "default",
			Annotations: map[string]string{
				grObservedAnnotation: "uuid-2:view-1:ONLINE:true",
			},
		},
	}
	r := grDoorbellCluster(t, &mysqlv1alpha1.ClusterStatus{TargetPrimary: "demo-1"}, pod, local)
	reconcile(t, r)

	got := &corev1.Pod{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "demo-1"}, got); err != nil {
		t.Fatal(err)
	}
	val := got.Annotations[grObservedAnnotation]
	if val != "uuid-2:view-2:ONLINE:true" {
		t.Fatalf("annotation = %q, want uuid-2:view-2:ONLINE:true", val)
	}
}
