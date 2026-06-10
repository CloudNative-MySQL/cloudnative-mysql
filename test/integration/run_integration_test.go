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

//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// runScript initialises an instance then runs it under the manager, which
// supervises mysqld and serves the control API. The control connection reaches
// mysqld over the administrative interface as the control user.
const runScript = `set -e
export MYSQL_ROOT_PASSWORD=rootpass MYSQL_CONTROL_PASSWORD=ctlpass MYSQL_APP_PASSWORD=apppass
cat > /tmp/my.cnf <<CFG
[mysqld]
server-id=1
gtid_mode=ON
enforce_gtid_consistency=ON
log_bin=binlog
log_replica_updates=ON
admin_address=127.0.0.1
admin_port=33062
CFG
manager instance initdb --mysqld=/usr/sbin/mysqld --config=/tmp/my.cnf \
  --data-dir=/var/lib/mysql --socket=/tmp/mysql.sock \
  --database=app --owner=appuser --control-user=control --server-version=8.0.36
exec manager instance run --mysqld=/usr/sbin/mysqld --config=/tmp/my.cnf \
  --data-dir=/var/lib/mysql --socket=/tmp/mysql.sock --server-version=8.0.36 \
  --instance-name=test-0 --control-user=control --web-addr=:8080
`

// TestRunServesControlAPI verifies that `instance run` starts mysqld and serves
// the control API: /healthz and /readyz report OK and /status reports a ready
// primary, with the control connection going over the admin interface.
func TestRunServesControlAPI(t *testing.T) {
	ctx := context.Background()

	req := testcontainers.ContainerRequest{
		FromDockerfile: testcontainers.FromDockerfile{
			Context:    buildInstanceContext(t),
			Dockerfile: "Dockerfile",
			KeepImage:  true,
		},
		ExposedPorts: []string{"8080/tcp"},
		Entrypoint:   []string{"bash", "-lc"},
		Cmd:          []string{runScript},
		WaitingFor: wait.ForHTTP("/readyz").WithPort("8080/tcp").
			WithStatusCodeMatcher(func(status int) bool { return status == http.StatusOK }).
			WithStartupTimeout(5 * time.Minute),
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("starting container: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := container.MappedPort(ctx, "8080")
	if err != nil {
		t.Fatal(err)
	}
	baseURL := fmt.Sprintf("http://%s:%d", host, mapped.Num())

	// /healthz must be OK.
	if code := getStatus(t, baseURL+"/healthz"); code != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", code)
	}

	// /status must report a ready primary.
	resp, err := http.Get(baseURL + "/status") //nolint:noctx // simple test request
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	var status struct {
		InstanceName string `json:"instanceName"`
		Role         string `json:"role"`
		IsReady      bool   `json:"isReady"`
	}
	if err := json.Unmarshal(body, &status); err != nil {
		t.Fatalf("decoding status %q: %v", body, err)
	}
	if status.Role != "primary" {
		t.Errorf("role = %q, want primary", status.Role)
	}
	if !status.IsReady {
		t.Errorf("instance should be ready: %s", body)
	}
	if status.InstanceName != "test-0" {
		t.Errorf("instanceName = %q, want test-0", status.InstanceName)
	}
}

func getStatus(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url) //nolint:noctx // simple test request
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}
