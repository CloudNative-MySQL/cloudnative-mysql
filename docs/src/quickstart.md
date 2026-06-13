---
title: "Quickstart"
description: "Build the CNMySQL images, deploy the operator, create a cluster, and verify write and read endpoints."
sidebar_position: 2
---

# Quickstart

This guide brings up CNMySQL in a development Kubernetes cluster and creates a
three-instance Percona Server for MySQL cluster.

The commands assume a Kind-style local environment and the default development
image names used by the repository.

## Prerequisites

- `go`
- `docker` or a compatible container tool
- `kubectl`
- `kind`
- `make`
- `cert-manager` in the target cluster, unless you install it as part of your
  local e2e setup

CNMySQL uses cert-manager-issued certificates for instance-manager mTLS and
MySQL TLS in the current implementation.

## Build images

Build the operator image:

```bash
make docker-build IMG=cnmysql-controller:dev
```

Build one instance image:

```bash
make docker-build-instance INSTANCE_VERSION=8.4
```

By default this creates an image named like:

```text
cnmysql-instance:8.4
```

For a local Kind cluster, load both images:

```bash
kind load docker-image cnmysql-controller:dev --name cnmysql-test-e2e
kind load docker-image cnmysql-instance:8.4 --name cnmysql-test-e2e
```

## Deploy the operator

Install CRDs and deploy the controller:

```bash
make install
make deploy IMG=cnmysql-controller:dev
```

Check the controller manager:

```bash
kubectl get pods -n cnmysql-system
```

## Create a cluster

Apply a minimal three-instance cluster:

```yaml
apiVersion: mysql.cloudnative-mysql.io/v1alpha1
kind: Cluster
metadata:
  name: cluster-sample
spec:
  instances: 3
  imageName: cnmysql-instance:8.4
  storage:
    size: 10Gi
  mysql:
    binlogFormat: ROW
  bootstrap:
    initdb:
      database: app
      owner: app
```

Wait for readiness:

```bash
kubectl wait --for=condition=Ready cluster/cluster-sample --timeout=15m
kubectl get cluster cluster-sample
kubectl get pods -l mysql.cloudnative-mysql.io/cluster=cluster-sample
```

Expected topology:

- `cluster-sample-1`, `cluster-sample-2`, and `cluster-sample-3` Pods exist.
- One Pod has `mysql.cloudnative-mysql.io/role=primary`.
- The remaining ready Pods have `mysql.cloudnative-mysql.io/role=replica`.
- `status.readyInstances` is `3`.

## Connect through services

CNMySQL creates role-routed Services:

- `cluster-sample-rw`: read-write endpoint for the current primary.
- `cluster-sample-ro`: read-only endpoint for replicas.
- `cluster-sample-r`: read endpoint for any ready instance.

Inspect them:

```bash
kubectl get svc cluster-sample-rw cluster-sample-ro cluster-sample-r
```

The generated application credentials are stored in the application Secret. The
exact Secret name depends on the cluster plan; inspect the generated Secrets:

```bash
kubectl get secrets -l mysql.cloudnative-mysql.io/cluster=cluster-sample
```

For quick smoke testing, exec from a MySQL client image or a temporary debug Pod
inside the same namespace and connect to `cluster-sample-rw`.

## Scale the cluster

Increase the instance count:

```bash
kubectl patch cluster cluster-sample --type merge -p '{"spec":{"instances":4}}'
kubectl wait --for=condition=Ready cluster/cluster-sample --timeout=15m
```

Scale down:

```bash
kubectl patch cluster cluster-sample --type merge -p '{"spec":{"instances":1}}'
```

Scale-down deletes replica Pods highest ordinal first and retains their PVCs.
Delete retained PVCs only after you are sure the data is no longer needed.

## Take a backup

Configure an object store on the Cluster or Backup, then create:

```yaml
apiVersion: mysql.cloudnative-mysql.io/v1alpha1
kind: Backup
metadata:
  name: backup-sample
spec:
  cluster:
    name: cluster-sample
  method: xtrabackup
  target: prefer-standby
  online: true
```

Watch it:

```bash
kubectl get backup backup-sample -w
kubectl describe backup backup-sample
```

## Clean up

Delete the Cluster:

```bash
kubectl delete cluster cluster-sample
```

Deleting a `Backup` object does not currently delete S3 objects. Remove object
store data manually or through external lifecycle policies until CNMySQL remote
backup cleanup and retention GC are implemented.

## Next steps

- Configure object storage and backups.
- Enable continuous archiving before relying on PITR.
- Read the operations runbook before testing switchover or failover.
