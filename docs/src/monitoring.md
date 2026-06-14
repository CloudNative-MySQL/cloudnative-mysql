---
title: "Monitoring"
description: "Prometheus metrics and PodMonitor integration."
sidebar_position: 12
---

# Monitoring

CNMySQL instances expose Prometheus metrics on port `9187` at `/metrics`.
The metrics server is separate from the mTLS control API and the health probe
server.

The current exporter publishes built-in Go runtime metrics plus MySQL global
status metrics from `SHOW GLOBAL STATUS`. More MySQL scraper families and custom
query loading are planned as M13.1 continues.

## PodMonitor

When the Prometheus Operator CRDs are installed, CNMySQL can create an owned
`PodMonitor` for a cluster:

```yaml
apiVersion: mysql.cloudnative-mysql.io/v1alpha1
kind: Cluster
metadata:
  name: cluster-sample
spec:
  monitoring:
    enablePodMonitor: true
```

The generated `PodMonitor` selects pods with:

```yaml
cnmysql.io/cluster: <cluster-name>
```

and scrapes the named container port `metrics`.

## Authenticated metrics over TLS

By default the metrics endpoint is served over plain HTTP. Setting
`spec.monitoring.tls.enabled` switches it to **mutual TLS**, reusing the same
PKI as the control API: the instance presents its server certificate and
requires the scraper to present a client certificate signed by the cluster CA.

```yaml
apiVersion: mysql.cloudnative-mysql.io/v1alpha1
kind: Cluster
metadata:
  name: cluster-sample
spec:
  monitoring:
    enablePodMonitor: true
    tls:
      enabled: true
```

No extra certificates are needed — the instance Pods already mount the
`server-tls` certificate and the `client-ca` bundle. When a `PodMonitor` is
generated, CNMySQL wires the scrape-side TLS configuration automatically:

- the endpoint scheme becomes `https`;
- the cluster CA secret (`<cluster>-ca`, key `ca.crt`) verifies the server cert;
- the operator client certificate (`<cluster>-client-tls`) authenticates the
  scrape;
- the read Service hostname (`<cluster>-r.<namespace>.svc`) — a SAN present on
  every instance certificate — is used as the verified server name.

Prometheus must be able to read those secrets in the cluster's namespace to
mount the client certificate and CA.

## Custom Queries

`customQueriesConfigMap`, `customQueriesSecret`, `disableDefaultQueries`, and
`metricsQueriesTTL` are API fields for the custom-query collector. The endpoint
is available now; query loading and default query injection are the next M13.1
slice.
