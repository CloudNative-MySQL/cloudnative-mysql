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
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mysqlv1alpha1 "github.com/yyewolf/cnmysql/api/v1alpha1"
	"github.com/yyewolf/cnmysql/pkg/management/mysql/replication"
	"github.com/yyewolf/cnmysql/pkg/management/mysql/webserver"
)

// HTTPControlClient drives the mTLS control API exposed by the instance manager.
type HTTPControlClient struct {
	Client     client.Client
	HTTPClient *http.Client
}

// HTTPStatusClient reads instance status through the mTLS control API exposed
// by the instance manager.
type HTTPStatusClient = HTTPControlClient

// Status fetches /status from the per-instance Service.
func (c *HTTPControlClient) Status(ctx context.Context, cluster *mysqlv1alpha1.Cluster, instanceName string) (*webserver.Status, error) {
	resp, err := c.do(ctx, cluster, instanceName, http.MethodGet, "/status", nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("instance status returned %s", resp.Status)
	}
	var status webserver.Status
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, err
	}
	return &status, nil
}

// Promote asks the instance manager to promote an instance to primary.
func (c *HTTPControlClient) Promote(ctx context.Context, cluster *mysqlv1alpha1.Cluster, instanceName string) error {
	return c.action(ctx, cluster, instanceName, "/promote", nil)
}

// Demote asks the instance manager to make an instance read-only.
func (c *HTTPControlClient) Demote(ctx context.Context, cluster *mysqlv1alpha1.Cluster, instanceName string) error {
	return c.action(ctx, cluster, instanceName, "/demote", nil)
}

// ConfigureReplica points an instance at the requested source and starts
// replication.
func (c *HTTPControlClient) ConfigureReplica(ctx context.Context, cluster *mysqlv1alpha1.Cluster, instanceName string, source replication.SourceOptions) error {
	return c.action(ctx, cluster, instanceName, "/replica/source", webserver.ConfigureReplicaRequest{Source: source})
}

func (c *HTTPControlClient) action(ctx context.Context, cluster *mysqlv1alpha1.Cluster, instanceName, path string, body any) error {
	resp, err := c.do(ctx, cluster, instanceName, http.MethodPost, path, body)
	if err != nil {
		return err
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("instance action %s on %s returned %s", path, instanceName, resp.Status)
	}
	return nil
}

func (c *HTTPControlClient) do(ctx context.Context, cluster *mysqlv1alpha1.Cluster, instanceName, method, path string, body any) (*http.Response, error) {
	conn := statusTLS{
		ServiceName:     instanceName,
		CASecretName:    cluster.Name + "-ca",
		ClientTLSSecret: cluster.Name + "-client-tls",
	}
	if certs := cluster.Spec.Certificates; certs != nil {
		if certs.ClientCASecret != "" {
			conn.CASecretName = certs.ClientCASecret
		}
		if certs.ReplicationTLSSecret != "" {
			conn.ClientTLSSecret = certs.ReplicationTLSSecret
		}
	}

	transport, err := c.transport(ctx, cluster.Namespace, conn)
	if err != nil {
		return nil, err
	}
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}
	clientCopy := *httpClient
	clientCopy.Transport = transport

	var requestBody *bytes.Reader
	if body == nil {
		requestBody = bytes.NewReader(nil)
	} else {
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		requestBody = bytes.NewReader(payload)
	}
	url := fmt.Sprintf("https://%s.%s.svc:8080%s", conn.ServiceName, cluster.Namespace, path)
	req, err := http.NewRequestWithContext(ctx, method, url, requestBody)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return clientCopy.Do(req)
}

// statusTLS holds the names needed to build the mTLS connection to an instance
// manager's control API.
type statusTLS struct {
	ServiceName     string
	CASecretName    string
	ClientTLSSecret string
}

func (c *HTTPControlClient) transport(ctx context.Context, namespace string, conn statusTLS) (*http.Transport, error) {
	caSecret := &corev1.Secret{}
	if err := c.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: conn.CASecretName}, caSecret); err != nil {
		return nil, err
	}
	clientSecret := &corev1.Secret{}
	if err := c.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: conn.ClientTLSSecret}, clientSecret); err != nil {
		return nil, err
	}

	cert, err := tls.X509KeyPair(clientSecret.Data[corev1.TLSCertKey], clientSecret.Data[corev1.TLSPrivateKeyKey])
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caSecret.Data["ca.crt"]) {
		return nil, fmt.Errorf("secret %s does not contain a valid ca.crt", conn.CASecretName)
	}

	return &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			ServerName:   conn.ServiceName + "." + namespace + ".svc",
			Certificates: []tls.Certificate{cert},
			RootCAs:      roots,
		},
	}, nil
}
