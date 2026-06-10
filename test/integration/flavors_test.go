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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/yyewolf/cnmysql/pkg/management/mysql/version"
)

// flavor describes a supported MySQL/Percona version under test, including the
// images to build from and the version-specific traits the tests must account
// for.
type flavor struct {
	// name is the subtest name.
	name string
	// perconaImage is the Percona Server base image.
	perconaImage string
	// xtrabackupImage is the matching XtraBackup image.
	xtrabackupImage string
	// version is passed to the manager as --server-version.
	version string
	// modernXtrabackup is true for the 8.0+ XtraBackup images, which bundle
	// private libraries under /usr/lib/private; the 2.4 series does not.
	modernXtrabackup bool
	// hasAdminInterface is true for servers with the administrative interface
	// (8.0.14+); older servers reach the control connection over the socket.
	hasAdminInterface bool
}

// flavors is the matrix of MySQL versions the operator targets and for which an
// upstream Percona image exists.
//
// Percona Server 9.x has no published image yet, so it is not covered here; add
// it once percona/percona-server:9.x and a matching XtraBackup image ship.
var flavors = []flavor{
	{
		name:              "8.0",
		perconaImage:      "percona/percona-server:8.0",
		xtrabackupImage:   "percona/percona-xtrabackup:8.0",
		version:           "8.0.36",
		modernXtrabackup:  true,
		hasAdminInterface: true,
	},
	{
		name:              "8.4",
		perconaImage:      "percona/percona-server:8.4",
		xtrabackupImage:   "percona/percona-xtrabackup:8.4",
		version:           "8.4.0",
		modernXtrabackup:  true,
		hasAdminInterface: true,
	},
	{
		name:              "5.6",
		perconaImage:      "percona/percona-server:5.6",
		xtrabackupImage:   "percona/percona-xtrabackup:2.4",
		version:           "5.6.51",
		modernXtrabackup:  false,
		hasAdminInterface: false,
	},
}

// gtidArgs returns the mysqld command-line flags enabling GTID replication for
// the flavor, accounting for the 8.0 rename of log_slave_updates.
func (f flavor) gtidArgs(t *testing.T) string {
	t.Helper()
	v, err := version.Parse(f.version)
	if err != nil {
		t.Fatal(err)
	}
	updates := "--log-slave-updates"
	if v.HasLogReplicaUpdates() {
		updates = "--log-replica-updates=ON"
	}
	return "--gtid-mode=ON --enforce-gtid-consistency=ON --log-bin=binlog " +
		updates + " --binlog-format=ROW"
}

// myCnf renders a minimal [mysqld] configuration for the flavor: GTID
// replication and, where supported, the administrative interface.
func (f flavor) myCnf(t *testing.T, serverID int) string {
	t.Helper()
	v, err := version.Parse(f.version)
	if err != nil {
		t.Fatal(err)
	}
	updates := "log_slave_updates=ON"
	if v.HasLogReplicaUpdates() {
		updates = "log_replica_updates=ON"
	}
	cfg := fmt.Sprintf(`[mysqld]
server-id=%d
gtid_mode=ON
enforce_gtid_consistency=ON
log_bin=binlog
%s
binlog_format=ROW
`, serverID, updates)
	if f.hasAdminInterface {
		cfg += "admin_address=127.0.0.1\nadmin_port=33062\n"
	}
	return cfg
}

// buildInstanceContext compiles the manager binary for the container platform
// and writes it next to a thin Dockerfile that layers it (and XtraBackup) onto
// the flavor's Percona image. Prebuilding avoids compiling Go inside Docker.
func buildInstanceContext(t *testing.T, f flavor) string {
	t.Helper()
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	build := exec.Command("go", "build", "-o", filepath.Join(dir, "manager"), "./cmd/manager")
	build.Dir = repoRoot
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building manager binary: %v\n%s", err, out)
	}

	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(f.dockerfile()), 0o600); err != nil {
		t.Fatalf("writing Dockerfile: %v", err)
	}
	return dir
}

// dockerfile renders the thin image Dockerfile for the flavor, copying the
// XtraBackup tooling and its libraries from the matching XtraBackup image.
func (f flavor) dockerfile() string {
	df := fmt.Sprintf("FROM %s\n", f.perconaImage)
	df += fmt.Sprintf("COPY --from=%s /usr/bin/xtrabackup /usr/bin/xbstream /usr/bin/\n", f.xtrabackupImage)
	if f.modernXtrabackup {
		df += fmt.Sprintf("COPY --from=%s /usr/lib64/libev.so.4 /usr/lib64/libev.so.4.0.0 /usr/lib64/\n", f.xtrabackupImage)
		df += fmt.Sprintf("COPY --from=%s /usr/lib/private/ /usr/lib/private/\n", f.xtrabackupImage)
	}
	df += "COPY manager /usr/local/bin/manager\n"
	df += "ENTRYPOINT [\"/usr/local/bin/manager\"]\n"
	return df
}
