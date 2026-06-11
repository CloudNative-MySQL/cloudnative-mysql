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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	mysqlv1alpha1 "github.com/yyewolf/cnmysql/api/v1alpha1"
	mysqlconfig "github.com/yyewolf/cnmysql/pkg/management/mysql/config"
	"github.com/yyewolf/cnmysql/pkg/management/mysql/webserver"
)

const (
	defaultInstanceImage = "cnmysql-instance:8.0"

	clusterLabel  = "mysql.cloudnative-mysql.io/cluster"
	instanceLabel = "mysql.cloudnative-mysql.io/instance"
	roleLabel     = "mysql.cloudnative-mysql.io/role"

	conditionReady       = "Ready"
	conditionProgressing = "Progressing"

	phasePending      = "Pending"
	phaseProvisioning = "Provisioning"
	phaseReady        = "Ready"
	phaseBlocked      = "Blocked"

	dataDir       = "/var/lib/mysql"
	socketPath    = "/var/run/mysqld/mysqld.sock"
	configPath    = "/etc/mysql/my.cnf"
	serverTLSPath = "/etc/cnmysql/tls/server"
	clientCAPath  = "/etc/cnmysql/tls/client-ca"
)

var (
	issuerGVK = schema.GroupVersionKind{
		Group:   "cert-manager.io",
		Version: "v1",
		Kind:    "Issuer",
	}
	certificateGVK = schema.GroupVersionKind{
		Group:   "cert-manager.io",
		Version: "v1",
		Kind:    "Certificate",
	}
)

// InstanceStatusClient reads the status served by an instance manager.
type InstanceStatusClient interface {
	Status(ctx context.Context, cluster *mysqlv1alpha1.Cluster, instanceName string) (*webserver.Status, error)
}

// ClusterReconciler reconciles a Cluster object.
type ClusterReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	StatusClient InstanceStatusClient
}

type clusterPlan struct {
	Image             string
	ServerVersion     string
	InstanceName      string
	ConfigMapName     string
	DataPVCName       string
	ServiceName       string
	RootSecretName    string
	AppSecretName     string
	ReplicationSecret string
	ControlSecretName string
	SelfSignedIssuer  string
	CAIssuer          string
	CASecretName      string
	ServerTLSSecret   string
	ClientTLSSecret   string
}

// +kubebuilder:rbac:groups=mysql.cloudnative-mysql.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=mysql.cloudnative-mysql.io,resources=clusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=mysql.cloudnative-mysql.io,resources=clusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=mysql.cloudnative-mysql.io,resources=imagecatalogs,verbs=get;list;watch
// +kubebuilder:rbac:groups=mysql.cloudnative-mysql.io,resources=clusterimagecatalogs,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps;pods;pods/status;persistentvolumeclaims;secrets;services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cert-manager.io,resources=issuers;certificates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile creates the first primary instance for a fresh single-instance
// Cluster. Replicas, traffic services, backup and failover are intentionally
// deferred to later milestones.
func (r *ClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	cluster := &mysqlv1alpha1.Cluster{}
	if err := r.Get(ctx, req.NamespacedName, cluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	cluster.SetDefaults()

	if reason := unsupportedReason(cluster); reason != "" {
		log.Info("Cluster shape is not supported by M3", "reason", reason)
		return ctrl.Result{}, r.patchStatus(ctx, cluster, observedCluster{
			Phase:       phaseBlocked,
			PhaseReason: reason,
			Ready:       false,
			Progressing: false,
		})
	}

	plan, err := r.buildPlan(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, r.patchStatus(ctx, cluster, observedCluster{
			Phase:       phaseBlocked,
			PhaseReason: err.Error(),
			Ready:       false,
			Progressing: false,
		})
	}

	if err := r.ensureCredentials(ctx, cluster, plan); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureCertificates(ctx, cluster, plan); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureConfigMap(ctx, cluster, plan); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensurePVC(ctx, cluster, plan); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureService(ctx, cluster, plan); err != nil {
		return ctrl.Result{}, err
	}

	certsReady, err := r.certSecretsReady(ctx, cluster, plan)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !certsReady {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, r.patchStatus(ctx, cluster, observedCluster{
			Phase:       phaseProvisioning,
			PhaseReason: "Waiting for cert-manager certificates",
			Ready:       false,
			Progressing: true,
			Plan:        plan,
		})
	}

	if err := r.ensurePod(ctx, cluster, plan); err != nil {
		return ctrl.Result{}, err
	}

	observed, err := r.observe(ctx, cluster, plan)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.patchStatus(ctx, cluster, observed); err != nil {
		return ctrl.Result{}, err
	}
	if !observed.Ready {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&mysqlv1alpha1.Cluster{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.Service{}).
		Named("cluster").
		Complete(r)
}

type observedCluster struct {
	Phase       string
	PhaseReason string
	Ready       bool
	Progressing bool
	Plan        clusterPlan
	Status      *webserver.Status
}

func unsupportedReason(cluster *mysqlv1alpha1.Cluster) string {
	switch {
	case cluster.Spec.Instances != 1:
		return "M3 supports only spec.instances=1; replicas are kept for M4"
	case cluster.Spec.Bootstrap == nil || cluster.Spec.Bootstrap.InitDB == nil:
		return "M3 supports only bootstrap.initdb; recovery is kept for M6"
	case cluster.Spec.Bootstrap.Recovery != nil:
		return "bootstrap.recovery is kept for M6"
	case cluster.Spec.Replica != nil:
		return "replica clusters are kept for M4"
	case cluster.Spec.BinlogStorage != nil:
		return "separate binlog storage is kept for M6"
	}
	return ""
}

func (r *ClusterReconciler) buildPlan(ctx context.Context, cluster *mysqlv1alpha1.Cluster) (clusterPlan, error) {
	image, err := r.resolveImage(ctx, cluster)
	if err != nil {
		return clusterPlan{}, err
	}

	certs := cluster.Spec.Certificates
	plan := clusterPlan{
		Image:             image,
		ServerVersion:     cluster.Spec.MySQL.ServerVersion,
		InstanceName:      cluster.Name + "-1",
		ConfigMapName:     cluster.Name + "-config",
		DataPVCName:       cluster.Name + "-1",
		ServiceName:       cluster.Name + "-1",
		RootSecretName:    cluster.Name + "-root",
		AppSecretName:     cluster.Name + "-app",
		ReplicationSecret: cluster.Name + "-replication",
		ControlSecretName: cluster.Name + "-control",
		SelfSignedIssuer:  cluster.Name + "-selfsigned",
		CAIssuer:          cluster.Name + "-ca",
		CASecretName:      cluster.Name + "-ca",
		ServerTLSSecret:   cluster.Name + "-server-tls",
		ClientTLSSecret:   cluster.Name + "-client-tls",
	}
	if cluster.Spec.RootPasswordSecret != nil && cluster.Spec.RootPasswordSecret.Name != "" {
		plan.RootSecretName = cluster.Spec.RootPasswordSecret.Name
	}
	if initdb := cluster.Spec.Bootstrap.InitDB; initdb.Secret != nil && initdb.Secret.Name != "" {
		plan.AppSecretName = initdb.Secret.Name
	}
	if certs != nil {
		if certs.ServerCASecret != "" {
			plan.CASecretName = certs.ServerCASecret
		}
		if certs.ClientCASecret != "" {
			plan.CASecretName = certs.ClientCASecret
		}
		if certs.ServerTLSSecret != "" {
			plan.ServerTLSSecret = certs.ServerTLSSecret
		}
		if certs.ReplicationTLSSecret != "" {
			plan.ClientTLSSecret = certs.ReplicationTLSSecret
		}
	}
	return plan, nil
}

func (r *ClusterReconciler) resolveImage(ctx context.Context, cluster *mysqlv1alpha1.Cluster) (string, error) {
	if cluster.Spec.ImageName != "" {
		return cluster.Spec.ImageName, nil
	}
	if ref := cluster.Spec.ImageCatalogRef; ref != nil {
		switch ref.Kind {
		case "ImageCatalog", "":
			catalog := &mysqlv1alpha1.ImageCatalog{}
			if err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: ref.Name}, catalog); err != nil {
				return "", err
			}
			if image, ok := catalog.Spec.FindImageForMajor(ref.Major); ok {
				return image, nil
			}
		case "ClusterImageCatalog":
			catalog := &mysqlv1alpha1.ClusterImageCatalog{}
			if err := r.Get(ctx, types.NamespacedName{Name: ref.Name}, catalog); err != nil {
				return "", err
			}
			if image, ok := catalog.Spec.FindImageForMajor(ref.Major); ok {
				return image, nil
			}
		default:
			return "", fmt.Errorf("unsupported imageCatalogRef kind %q", ref.Kind)
		}
		return "", fmt.Errorf("no image for MySQL major %d in catalog %s", ref.Major, ref.Name)
	}
	return defaultInstanceImage, nil
}

func (r *ClusterReconciler) ensureCredentials(ctx context.Context, cluster *mysqlv1alpha1.Cluster, plan clusterPlan) error {
	if cluster.Spec.RootPasswordSecret == nil {
		if err := r.ensurePasswordSecret(ctx, cluster, plan.RootSecretName, map[string]string{"username": "root"}); err != nil {
			return err
		}
	}
	if initdb := cluster.Spec.Bootstrap.InitDB; initdb.Secret == nil {
		user := initdb.Owner
		if user == "" {
			user = "app"
		}
		if err := r.ensurePasswordSecret(ctx, cluster, plan.AppSecretName, map[string]string{"username": user}); err != nil {
			return err
		}
	}
	if err := r.ensurePasswordSecret(ctx, cluster, plan.ReplicationSecret, map[string]string{"username": "cnmysql_repl"}); err != nil {
		return err
	}
	return r.ensurePasswordSecret(ctx, cluster, plan.ControlSecretName, map[string]string{"username": "cnmysql_control"})
}

func (r *ClusterReconciler) ensurePasswordSecret(ctx context.Context, cluster *mysqlv1alpha1.Cluster, name string, data map[string]string) error {
	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: name}, secret)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	password, err := randomPassword()
	if err != nil {
		return err
	}
	stringData := map[string]string{"password": password}
	maps.Copy(stringData, data)
	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cluster.Namespace,
			Labels:    labelsFor(cluster, ""),
		},
		Type:       corev1.SecretTypeOpaque,
		StringData: stringData,
	}
	if err := controllerutil.SetControllerReference(cluster, secret, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, secret)
}

func randomPassword() (string, error) {
	var buf [24]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

func (r *ClusterReconciler) ensureCertificates(ctx context.Context, cluster *mysqlv1alpha1.Cluster, plan clusterPlan) error {
	if hasUserCertificates(cluster) {
		return nil
	}
	if err := r.ensureIssuer(ctx, cluster, plan.SelfSignedIssuer, map[string]any{
		"selfSigned": map[string]any{},
	}); err != nil {
		return err
	}
	if err := r.ensureCertificate(ctx, cluster, plan.CAIssuer, map[string]any{
		"secretName": plan.CASecretName,
		"isCA":       true,
		"commonName": cluster.Name + ".ca.cnmysql",
		"issuerRef": map[string]any{
			"name": plan.SelfSignedIssuer,
			"kind": "Issuer",
		},
	}); err != nil {
		return err
	}
	if err := r.ensureIssuer(ctx, cluster, plan.CAIssuer, map[string]any{
		"ca": map[string]any{
			"secretName": plan.CASecretName,
		},
	}); err != nil {
		return err
	}
	if err := r.ensureCertificate(ctx, cluster, cluster.Name+"-server", map[string]any{
		"secretName": plan.ServerTLSSecret,
		"commonName": plan.ServiceName + "." + cluster.Namespace + ".svc",
		"dnsNames": []any{
			plan.InstanceName,
			plan.ServiceName,
			plan.ServiceName + "." + cluster.Namespace,
			plan.ServiceName + "." + cluster.Namespace + ".svc",
			plan.ServiceName + "." + cluster.Namespace + ".svc.cluster.local",
		},
		"issuerRef": map[string]any{
			"name": plan.CAIssuer,
			"kind": "Issuer",
		},
	}); err != nil {
		return err
	}
	return r.ensureCertificate(ctx, cluster, cluster.Name+"-client", map[string]any{
		"secretName": plan.ClientTLSSecret,
		"commonName": "cnmysql-operator",
		"usages": []any{
			"client auth",
		},
		"issuerRef": map[string]any{
			"name": plan.CAIssuer,
			"kind": "Issuer",
		},
	})
}

func hasUserCertificates(cluster *mysqlv1alpha1.Cluster) bool {
	certs := cluster.Spec.Certificates
	return certs != nil &&
		certs.ServerTLSSecret != "" &&
		certs.ClientCASecret != "" &&
		certs.ReplicationTLSSecret != ""
}

func (r *ClusterReconciler) ensureIssuer(ctx context.Context, cluster *mysqlv1alpha1.Cluster, name string, spec map[string]any) error {
	issuer := &unstructured.Unstructured{}
	issuer.SetGroupVersionKind(issuerGVK)
	issuer.SetName(name)
	issuer.SetNamespace(cluster.Namespace)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, issuer, func() error {
		issuer.SetLabels(labelsFor(cluster, ""))
		if err := unstructured.SetNestedMap(issuer.Object, spec, "spec"); err != nil {
			return err
		}
		return controllerutil.SetControllerReference(cluster, issuer, r.Scheme)
	})
	return err
}

func (r *ClusterReconciler) ensureCertificate(ctx context.Context, cluster *mysqlv1alpha1.Cluster, name string, spec map[string]any) error {
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	cert.SetName(name)
	cert.SetNamespace(cluster.Namespace)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cert, func() error {
		cert.SetLabels(labelsFor(cluster, ""))
		if err := unstructured.SetNestedMap(cert.Object, spec, "spec"); err != nil {
			return err
		}
		return controllerutil.SetControllerReference(cluster, cert, r.Scheme)
	})
	return err
}

func (r *ClusterReconciler) ensureConfigMap(ctx context.Context, cluster *mysqlv1alpha1.Cluster, plan clusterPlan) error {
	rendered, err := renderMyCnf(cluster, plan)
	if err != nil {
		return err
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name:      plan.ConfigMapName,
		Namespace: cluster.Namespace,
	}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = labelsFor(cluster, "")
		cm.Data = map[string]string{"my.cnf": rendered}
		return controllerutil.SetControllerReference(cluster, cm, r.Scheme)
	})
	return err
}

func renderMyCnf(cluster *mysqlv1alpha1.Cluster, plan clusterPlan) (string, error) {
	semiSync := mysqlconfig.SemiSync{}
	if cluster.Spec.MySQL.SemiSync != nil {
		semiSync.Enabled = cluster.Spec.MySQL.SemiSync.Enabled
		semiSync.WaitForReplicaCount = cluster.Spec.MinSyncReplicas
		if cluster.Spec.MySQL.SemiSync.TimeoutMillis != nil {
			semiSync.TimeoutMillis = int(*cluster.Spec.MySQL.SemiSync.TimeoutMillis)
		}
	}
	return (&mysqlconfig.ServerConfig{
		ServerID:       1,
		Version:        plan.ServerVersion,
		Role:           mysqlconfig.RolePrimary,
		DataDir:        dataDir,
		Socket:         socketPath,
		Port:           3306,
		ReportHost:     plan.ServiceName,
		BinlogFormat:   cluster.Spec.MySQL.BinlogFormat,
		AdminAddress:   mysqlconfig.DefaultAdminAddress,
		AdminPort:      mysqlconfig.DefaultAdminPort,
		UserParameters: cluster.Spec.MySQL.Parameters,
		SemiSync:       semiSync,
	}).Render()
}

func (r *ClusterReconciler) ensurePVC(ctx context.Context, cluster *mysqlv1alpha1.Cluster, plan clusterPlan) error {
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name:      plan.DataPVCName,
		Namespace: cluster.Namespace,
	}}
	if err := r.Get(ctx, client.ObjectKeyFromObject(pvc), pvc); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		spec, err := pvcSpec(cluster.Spec.Storage)
		if err != nil {
			return err
		}
		pvc.Labels = labelsFor(cluster, plan.InstanceName)
		pvc.Spec = spec
		if err := controllerutil.SetControllerReference(cluster, pvc, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, pvc)
	}

	if cluster.Spec.Storage.Size == "" {
		return nil
	}
	desired, err := resource.ParseQuantity(cluster.Spec.Storage.Size)
	if err != nil {
		return err
	}
	current := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if current.Cmp(desired) >= 0 {
		return nil
	}
	before := pvc.DeepCopy()
	if pvc.Spec.Resources.Requests == nil {
		pvc.Spec.Resources.Requests = corev1.ResourceList{}
	}
	pvc.Spec.Resources.Requests[corev1.ResourceStorage] = desired
	return r.Patch(ctx, pvc, client.MergeFrom(before))
}

func pvcSpec(storage mysqlv1alpha1.StorageConfiguration) (corev1.PersistentVolumeClaimSpec, error) {
	spec := corev1.PersistentVolumeClaimSpec{
		AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
	}
	if storage.PersistentVolumeClaimTemplate != nil {
		spec = *storage.PersistentVolumeClaimTemplate.DeepCopy()
	}
	if storage.StorageClass != nil {
		spec.StorageClassName = storage.StorageClass
	}
	if storage.Size != "" {
		quantity, err := resource.ParseQuantity(storage.Size)
		if err != nil {
			return corev1.PersistentVolumeClaimSpec{}, err
		}
		if spec.Resources.Requests == nil {
			spec.Resources.Requests = corev1.ResourceList{}
		}
		spec.Resources.Requests[corev1.ResourceStorage] = quantity
	}
	if spec.Resources.Requests.Storage().IsZero() {
		return corev1.PersistentVolumeClaimSpec{}, fmt.Errorf("spec.storage.size or spec.storage.pvcTemplate.resources.requests.storage is required")
	}
	return spec, nil
}

func (r *ClusterReconciler) ensureService(ctx context.Context, cluster *mysqlv1alpha1.Cluster, plan clusterPlan) error {
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name:      plan.ServiceName,
		Namespace: cluster.Namespace,
	}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, service, func() error {
		service.Labels = labelsFor(cluster, plan.InstanceName)
		service.Spec.ClusterIP = corev1.ClusterIPNone
		service.Spec.Selector = map[string]string{instanceLabel: plan.InstanceName}
		service.Spec.Ports = []corev1.ServicePort{
			{Name: "mysql", Port: 3306, TargetPort: intstr.FromString("mysql")},
			{Name: "control", Port: 8080, TargetPort: intstr.FromString("control")},
		}
		return controllerutil.SetControllerReference(cluster, service, r.Scheme)
	})
	return err
}

func (r *ClusterReconciler) certSecretsReady(ctx context.Context, cluster *mysqlv1alpha1.Cluster, plan clusterPlan) (bool, error) {
	for _, name := range []string{plan.CASecretName, plan.ServerTLSSecret, plan.ClientTLSSecret} {
		secret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: name}, secret); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
	}
	return true, nil
}

func (r *ClusterReconciler) ensurePod(ctx context.Context, cluster *mysqlv1alpha1.Cluster, plan clusterPlan) error {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      plan.InstanceName,
		Namespace: cluster.Namespace,
	}}
	if err := r.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		pod.Labels = labelsFor(cluster, plan.InstanceName)
		pod.Annotations = map[string]string{"cnmysql.cloudnative-mysql.io/config-map": plan.ConfigMapName}
		pod.Spec = r.podSpec(cluster, plan)
		if err := controllerutil.SetControllerReference(cluster, pod, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, pod)
	}
	return nil
}

func (r *ClusterReconciler) podSpec(cluster *mysqlv1alpha1.Cluster, plan clusterPlan) corev1.PodSpec {
	initdb := cluster.Spec.Bootstrap.InitDB
	podSpec := corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyAlways,
		Volumes: []corev1.Volume{
			{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: plan.DataPVCName}}},
			{Name: "run", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: plan.ConfigMapName}}}},
			{Name: "server-tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: plan.ServerTLSSecret}}},
			{Name: "client-ca", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: plan.CASecretName}}},
		},
		InitContainers: []corev1.Container{{
			Name:            "initdb",
			Image:           plan.Image,
			ImagePullPolicy: cluster.Spec.ImagePullPolicy,
			Args:            initdbArgs(initdb),
			Env:             initdbEnv(plan),
			VolumeMounts:    volumeMounts(),
			Resources:       cluster.Spec.Resources,
			SecurityContext: cluster.Spec.SecurityContext,
		}},
		Containers: []corev1.Container{{
			Name:            "mysql",
			Image:           plan.Image,
			ImagePullPolicy: cluster.Spec.ImagePullPolicy,
			Args:            runArgs(),
			Env:             runEnv(plan),
			EnvFrom:         cluster.Spec.EnvFrom,
			Ports: []corev1.ContainerPort{
				{Name: "mysql", ContainerPort: 3306},
				{Name: "control", ContainerPort: 8080},
			},
			VolumeMounts:    volumeMounts(),
			Resources:       cluster.Spec.Resources,
			SecurityContext: cluster.Spec.SecurityContext,
			ReadinessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{
					Port: intstr.FromString("control"),
				}},
				PeriodSeconds: 10,
			},
			LivenessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{
					Port: intstr.FromString("control"),
				}},
				PeriodSeconds: 30,
			},
		}},
		NodeSelector:              cluster.Spec.Affinity.NodeSelector,
		Affinity:                  affinity(cluster),
		Tolerations:               cluster.Spec.Affinity.Tolerations,
		TopologySpreadConstraints: cluster.Spec.TopologySpreadConstraints,
		PriorityClassName:         cluster.Spec.PriorityClassName,
		SchedulerName:             cluster.Spec.SchedulerName,
		SecurityContext:           podSecurityContext(cluster),
	}
	for _, pullSecret := range cluster.Spec.ImagePullSecrets {
		podSpec.ImagePullSecrets = append(podSpec.ImagePullSecrets, corev1.LocalObjectReference{Name: pullSecret.Name})
	}
	podSpec.Containers[0].Env = append(podSpec.Containers[0].Env, cluster.Spec.Env...)
	return podSpec
}

func initdbArgs(initdb *mysqlv1alpha1.BootstrapInitDB) []string {
	args := []string{
		"instance", "initdb",
		"--mysqld=/usr/sbin/mysqld",
		"--config=" + configPath,
		"--data-dir=" + dataDir,
		"--socket=" + socketPath,
		"--server-version=$(MYSQL_VERSION)",
		"--replication-user=cnmysql_repl",
		"--replication-require-x509",
		"--control-user=cnmysql_control",
	}
	if initdb.Database != "" {
		args = append(args, "--database="+initdb.Database)
	}
	if initdb.Owner != "" {
		args = append(args, "--owner="+initdb.Owner)
	}
	if initdb.CharacterSet != "" {
		args = append(args, "--character-set="+initdb.CharacterSet)
	}
	if initdb.Collation != "" {
		args = append(args, "--collation="+initdb.Collation)
	}
	return args
}

func runArgs() []string {
	return []string{
		"instance", "run",
		"--mysqld=/usr/sbin/mysqld",
		"--config=" + configPath,
		"--data-dir=" + dataDir,
		"--socket=" + socketPath,
		"--server-version=$(MYSQL_VERSION)",
		"--instance-name=$(POD_NAME)",
		"--control-user=cnmysql_control",
		"--admin-address=" + mysqlconfig.DefaultAdminAddress,
		fmt.Sprintf("--admin-port=%d", mysqlconfig.DefaultAdminPort),
		"--web-addr=:8080",
		"--tls-cert=" + serverTLSPath + "/tls.crt",
		"--tls-key=" + serverTLSPath + "/tls.key",
		"--tls-client-ca=" + clientCAPath + "/ca.crt",
	}
}

func initdbEnv(plan clusterPlan) []corev1.EnvVar {
	env := runEnv(plan)
	env = append(env,
		secretEnv("MYSQL_ROOT_PASSWORD", plan.RootSecretName),
		secretEnv("MYSQL_APP_PASSWORD", plan.AppSecretName),
		secretEnv("MYSQL_REPLICATION_PASSWORD", plan.ReplicationSecret),
	)
	return env
}

func runEnv(plan clusterPlan) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "MYSQL_VERSION", Value: plan.ServerVersion},
		{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
		secretEnv("MYSQL_CONTROL_PASSWORD", plan.ControlSecretName),
	}
}

func secretEnv(name, secretName string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: name,
		ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
			Key:                  "password",
		}},
	}
}

func volumeMounts() []corev1.VolumeMount {
	return []corev1.VolumeMount{
		{Name: "data", MountPath: dataDir},
		{Name: "run", MountPath: "/var/run/mysqld"},
		{Name: "config", MountPath: configPath, SubPath: "my.cnf", ReadOnly: true},
		{Name: "server-tls", MountPath: serverTLSPath, ReadOnly: true},
		{Name: "client-ca", MountPath: clientCAPath, ReadOnly: true},
	}
}

func affinity(cluster *mysqlv1alpha1.Cluster) *corev1.Affinity {
	if cluster.Spec.Affinity.NodeAffinity == nil &&
		cluster.Spec.Affinity.AdditionalPodAffinity == nil &&
		cluster.Spec.Affinity.AdditionalPodAntiAffinity == nil {
		return nil
	}
	return &corev1.Affinity{
		NodeAffinity:    cluster.Spec.Affinity.NodeAffinity,
		PodAffinity:     cluster.Spec.Affinity.AdditionalPodAffinity,
		PodAntiAffinity: cluster.Spec.Affinity.AdditionalPodAntiAffinity,
	}
}

func podSecurityContext(cluster *mysqlv1alpha1.Cluster) *corev1.PodSecurityContext {
	if cluster.Spec.PodSecurityContext != nil {
		return cluster.Spec.PodSecurityContext
	}
	runAsNonRoot := true
	runAsUser := int64(1001)
	fsGroup := int64(0)
	return &corev1.PodSecurityContext{
		RunAsNonRoot: &runAsNonRoot,
		RunAsUser:    &runAsUser,
		FSGroup:      &fsGroup,
	}
}

func (r *ClusterReconciler) observe(ctx context.Context, cluster *mysqlv1alpha1.Cluster, plan clusterPlan) (observedCluster, error) {
	observed := observedCluster{
		Phase:       phasePending,
		PhaseReason: "Waiting for Pod",
		Ready:       false,
		Progressing: true,
		Plan:        plan,
	}
	pod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: plan.InstanceName}, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return observed, nil
		}
		return observedCluster{}, err
	}
	if !podReady(pod) {
		observed.Phase = phaseProvisioning
		observed.PhaseReason = "Waiting for Pod readiness"
		return observed, nil
	}

	statusClient := r.StatusClient
	if statusClient == nil {
		statusClient = &HTTPStatusClient{Client: r.Client}
	}
	status, err := statusClient.Status(ctx, cluster, plan.InstanceName)
	if err != nil {
		observed.Phase = phaseProvisioning
		observed.PhaseReason = "Waiting for instance status: " + err.Error()
		return observed, nil
	}
	observed.Status = status
	observed.Ready = status.IsReady
	observed.Progressing = !status.IsReady
	if status.IsReady {
		observed.Phase = phaseReady
		observed.PhaseReason = "Instance is ready"
	} else {
		observed.Phase = phaseProvisioning
		observed.PhaseReason = "Instance manager reported not ready"
	}
	return observed, nil
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func (r *ClusterReconciler) patchStatus(ctx context.Context, cluster *mysqlv1alpha1.Cluster, observed observedCluster) error {
	latest := &mysqlv1alpha1.Cluster{}
	key := types.NamespacedName{Namespace: cluster.Namespace, Name: cluster.Name}
	if err := r.Get(ctx, key, latest); err != nil {
		return err
	}
	before := latest.DeepCopy()
	if observed.Plan.InstanceName != "" {
		latest.Status.Instances = 1
		latest.Status.InstanceNames = []string{observed.Plan.InstanceName}
		latest.Status.CurrentPrimary = observed.Plan.InstanceName
		latest.Status.LatestGeneratedNode = 1
		latest.Status.Image = observed.Plan.Image
	} else {
		latest.Status.Instances = latest.Spec.Instances
		latest.Status.InstanceNames = nil
		latest.Status.CurrentPrimary = ""
		latest.Status.LatestGeneratedNode = 0
		latest.Status.Image = ""
	}
	latest.Status.ObservedGeneration = latest.Generation
	latest.Status.Phase = observed.Phase
	latest.Status.PhaseReason = observed.PhaseReason
	if observed.Ready {
		latest.Status.ReadyInstances = 1
		now := metav1.Now().Format(time.RFC3339)
		if latest.Status.CurrentPrimaryTimestamp == "" {
			latest.Status.CurrentPrimaryTimestamp = now
		}
	} else {
		latest.Status.ReadyInstances = 0
	}
	if observed.Status != nil {
		latest.Status.GTIDExecutedByInstance = map[string]string{
			observed.Plan.InstanceName: observed.Status.GTIDExecuted,
		}
	}
	apimeta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
		Type:               conditionReady,
		Status:             conditionStatus(observed.Ready),
		Reason:             observed.Phase,
		Message:            observed.PhaseReason,
		ObservedGeneration: latest.Generation,
	})
	apimeta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
		Type:               conditionProgressing,
		Status:             conditionStatus(observed.Progressing),
		Reason:             observed.Phase,
		Message:            observed.PhaseReason,
		ObservedGeneration: latest.Generation,
	})
	return r.Status().Patch(ctx, latest, client.MergeFrom(before))
}

func conditionStatus(ok bool) metav1.ConditionStatus {
	if ok {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

func labelsFor(cluster *mysqlv1alpha1.Cluster, instanceName string) map[string]string {
	labels := map[string]string{
		"app.kubernetes.io/name":      "cnmysql",
		"app.kubernetes.io/instance":  cluster.Name,
		"app.kubernetes.io/component": "mysql",
		clusterLabel:                  cluster.Name,
	}
	if cluster.Spec.InheritedMetadata != nil {
		maps.Copy(labels, cluster.Spec.InheritedMetadata.Labels)
	}
	if instanceName != "" {
		labels[instanceLabel] = instanceName
		labels[roleLabel] = "primary"
	}
	return labels
}
