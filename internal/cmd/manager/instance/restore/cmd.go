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

// Package restore implements `manager instance restore`: bootstrap a primary's
// data directory from a physical backup stored in object storage.
package restore

import (
	"github.com/spf13/cobra"

	"github.com/yyewolf/cnmysql/pkg/management/mysql/instance"
	"github.com/yyewolf/cnmysql/pkg/management/mysql/objectstore"
)

// NewCommand builds the `instance restore` command.
func NewCommand() *cobra.Command {
	var (
		xtrabackupPath string
		xbstreamPath   string
		backupDir      string
		dataDir        string
		bucket         string
		archiveKey     string
		metadataKey    string
		compress       bool
		verifyChecksum bool
	)

	cmd := &cobra.Command{
		Use:   "restore",
		Short: "Restore a physical backup from object storage into the data directory",
		Long: "Download an XtraBackup archive from S3-compatible object storage, " +
			"extract, prepare and restore it into the data directory. Idempotent: " +
			"a no-op when the data directory is already initialised. Object-store " +
			"credentials are read from the CNMYSQL_S3_* environment variables.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := objectstore.NewClientFromEnv()
			if err != nil {
				return err
			}
			return instance.Restore(cmd.Context(), instance.RestoreOptions{
				Store:          store,
				Bucket:         bucket,
				ArchiveKey:     archiveKey,
				MetadataKey:    metadataKey,
				BackupDir:      backupDir,
				DataDir:        dataDir,
				XBStreamPath:   xbstreamPath,
				XtrabackupPath: xtrabackupPath,
				Compress:       compress,
				VerifyChecksum: verifyChecksum,
			})
		},
	}

	cmd.Flags().StringVar(&xtrabackupPath, "xtrabackup", "xtrabackup", "Path to the xtrabackup binary")
	cmd.Flags().StringVar(&xbstreamPath, "xbstream", "xbstream", "Path to the xbstream binary")
	cmd.Flags().StringVar(&backupDir, "backup-dir", "", "Scratch directory to extract the archive into")
	cmd.Flags().StringVar(&dataDir, "data-dir", "/var/lib/mysql", "MySQL data directory")
	cmd.Flags().StringVar(&bucket, "bucket", "", "Source object-store bucket")
	cmd.Flags().StringVar(&archiveKey, "archive-key", "", "Object key of the xbstream archive")
	cmd.Flags().StringVar(&metadataKey, "metadata-key", "", "Object key of the backup metadata; when set, drives decompression and checksum verification")
	cmd.Flags().BoolVar(&compress, "compress", false, "The archive is compressed and must be decompressed after extraction (overridden by metadata when present)")
	cmd.Flags().BoolVar(&verifyChecksum, "verify-checksum", true, "Verify the downloaded archive against the metadata SHA256")

	return cmd
}
