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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	mysqlv1alpha1 "github.com/yyewolf/cnmysql/api/v1alpha1"
	"github.com/yyewolf/cnmysql/pkg/management/mysql/objectstore"
)

// backupDestinationCheck reports the outcome of the empty-archive safety check.
type backupDestinationCheck struct {
	// Blocked is a non-empty human-readable reason when the destination already
	// holds data and the fresh cluster must not adopt it.
	Blocked string
	// Retry is set when the destination could not be verified (e.g. the object
	// store is unreachable); the caller should requeue rather than block.
	Retry error
}

// checkBackupDestination guards a freshly bootstrapping cluster from adopting an
// object-store destination that already contains another cluster's backups.
//
// It mirrors CloudNativePG's empty-archive check: a fresh cluster pointed at a
// non-empty destination is kept out of service (Blocked) instead of silently
// overwriting existing data. The check only applies before the primary is
// established (status.currentPrimary unset) and never to a recovery bootstrap,
// whose destination is expected to already hold the backups it restores from.
func (r *ClusterReconciler) checkBackupDestination(
	ctx context.Context,
	cluster *mysqlv1alpha1.Cluster,
) backupDestinationCheck {
	// Only fresh, archiving clusters that have never established a primary.
	if cluster.Status.CurrentPrimary != "" {
		return backupDestinationCheck{}
	}
	if cluster.Spec.Bootstrap != nil && cluster.Spec.Bootstrap.Recovery != nil {
		return backupDestinationCheck{}
	}
	if cluster.Spec.Backup == nil || cluster.Spec.Backup.ObjectStore == nil {
		return backupDestinationCheck{}
	}

	store := cluster.Spec.Backup.ObjectStore
	cfg, err := r.objectStoreConfig(ctx, cluster.Namespace, store)
	if err != nil {
		return backupDestinationCheck{Retry: err}
	}
	client, err := objectstore.NewClient(cfg)
	if err != nil {
		return backupDestinationCheck{Retry: err}
	}

	prefix := objectstore.ClusterPrefix(*store, cluster.Name)
	empty, err := client.IsEmptyPrefix(ctx, store.Bucket, prefix)
	if err != nil {
		return backupDestinationCheck{Retry: err}
	}
	if !empty {
		return backupDestinationCheck{Blocked: fmt.Sprintf(
			"Backup destination s3://%s/%s is not empty; refusing to overwrite an existing archive. "+
				"Use a different cluster name or object-store path, or bootstrap with spec.bootstrap.recovery to restore it",
			store.Bucket, prefix)}
	}
	return backupDestinationCheck{}
}

// objectStoreConfig resolves an object store plus its secret-backed credentials
// into a client Config the operator can use directly.
func (r *ClusterReconciler) objectStoreConfig(
	ctx context.Context,
	namespace string,
	store *mysqlv1alpha1.S3ObjectStore,
) (objectstore.Config, error) {
	var accessKeyID, secretAccessKey, sessionToken string
	creds := store.Credentials
	if creds.AccessKeyID != nil {
		value, err := r.secretValue(ctx, namespace, *creds.AccessKeyID)
		if err != nil {
			return objectstore.Config{}, err
		}
		accessKeyID = value
	}
	if creds.SecretAccessKey != nil {
		value, err := r.secretValue(ctx, namespace, *creds.SecretAccessKey)
		if err != nil {
			return objectstore.Config{}, err
		}
		secretAccessKey = value
	}
	if creds.SessionToken != nil {
		value, err := r.secretValue(ctx, namespace, *creds.SessionToken)
		if err != nil {
			return objectstore.Config{}, err
		}
		sessionToken = value
	}
	return objectstore.ConfigFromStore(*store, accessKeyID, secretAccessKey, sessionToken), nil
}

func (r *ClusterReconciler) secretValue(
	ctx context.Context,
	namespace string,
	selector mysqlv1alpha1.SecretKeySelector,
) (string, error) {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: namespace, Name: selector.Name}
	if err := r.Get(ctx, key, secret); err != nil {
		return "", fmt.Errorf("reading secret %s/%s: %w", namespace, selector.Name, err)
	}
	value, ok := secret.Data[selector.Key]
	if !ok {
		return "", fmt.Errorf("secret %s/%s has no key %q", namespace, selector.Name, selector.Key)
	}
	return string(value), nil
}
