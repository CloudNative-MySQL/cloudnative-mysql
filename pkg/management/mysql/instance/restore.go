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

package instance

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/yyewolf/cnmysql/pkg/management/mysql/objectstore"
	"github.com/yyewolf/cnmysql/pkg/management/mysql/xtrabackup"
)

// RestoreOptions configures bootstrapping a primary's data directory from a
// physical backup held in object storage.
//
// Recovery is the object-store analogue of Join: it extracts the archive,
// prepares it, and restores it into DataDir. Unlike Join it configures no
// replication — recovery produces a standalone primary that replicas then clone
// from in the usual way.
type RestoreOptions struct {
	// Store reads the archive (and optional metadata) from object storage.
	Store *objectstore.Client
	// Bucket and ArchiveKey locate the xbstream archive.
	Bucket     string
	ArchiveKey string
	// MetadataKey, when set, is read first to discover the archive checksum and
	// whether it is compressed; it also lets recovery verify integrity.
	MetadataKey string
	// BackupDir is scratch space the archive is extracted into.
	BackupDir string
	// DataDir is the data directory to restore into.
	DataDir string
	// XBStreamPath is the xbstream binary (default "xbstream").
	XBStreamPath string
	// XtrabackupPath is the xtrabackup binary (default "xtrabackup").
	XtrabackupPath string
	// Compress forces decompression after extraction. When MetadataKey is set the
	// archive's recorded compression flag takes precedence.
	Compress bool
	// VerifyChecksum compares the downloaded archive against the metadata SHA256.
	VerifyChecksum bool
}

func (o *RestoreOptions) applyDefaults() {
	if o.XBStreamPath == "" {
		o.XBStreamPath = "xbstream"
	}
	if o.XtrabackupPath == "" {
		o.XtrabackupPath = defaultXtrabackupBinary
	}
}

// Restore downloads the archive from object storage, extracts and prepares it,
// then restores it into DataDir. It is idempotent: a no-op if the data
// directory is already initialised.
func Restore(ctx context.Context, opts RestoreOptions) error {
	opts.applyDefaults()
	log := logf.FromContext(ctx).WithName("instance-restore").WithValues(
		"dataDir", opts.DataDir,
		"backupDir", opts.BackupDir,
		"bucket", opts.Bucket,
		"archiveKey", opts.ArchiveKey,
	)
	log.Info("Starting restore from object store")

	if opts.Store == nil {
		return fmt.Errorf("restore: object-store client is required")
	}
	if opts.Bucket == "" || opts.ArchiveKey == "" {
		return fmt.Errorf("restore: bucket and archive key are required")
	}
	if opts.DataDir == "" || opts.BackupDir == "" {
		return fmt.Errorf("restore: data dir and backup dir are required")
	}

	if IsInitialized(opts.DataDir) {
		log.Info("Data directory already initialized")
		return nil
	}

	compress := opts.Compress
	var expectedSHA256 string
	if opts.MetadataKey != "" {
		var metadata objectstore.BackupMetadata
		if err := opts.Store.GetJSON(ctx, opts.Bucket, opts.MetadataKey, &metadata); err != nil {
			return fmt.Errorf("reading backup metadata: %w", err)
		}
		compress = metadata.Compressed
		expectedSHA256 = metadata.SHA256
		log.Info("Loaded backup metadata",
			"backupID", metadata.BackupID, "compressed", compress, "sizeBytes", metadata.SizeBytes)
	}

	if err := os.MkdirAll(opts.BackupDir, 0o750); err != nil {
		return fmt.Errorf("creating backup dir: %w", err)
	}

	// 1. Stream the archive out of object storage straight into `xbstream -x`,
	// checksumming in flight so it can be verified against the metadata.
	checksum, err := opts.extract(ctx)
	if err != nil {
		return err
	}
	if opts.VerifyChecksum && expectedSHA256 != "" && checksum != expectedSHA256 {
		return fmt.Errorf("restore: archive checksum mismatch: got %s, want %s", checksum, expectedSHA256)
	}

	// 2. Optionally decompress the extracted archive.
	if compress {
		decompressArgs, err := xtrabackup.DecompressArgs(opts.BackupDir)
		if err != nil {
			return err
		}
		log.Info("Decompressing backup")
		if err := runCommand(ctx, opts.XtrabackupPath, decompressArgs); err != nil {
			return fmt.Errorf("xtrabackup decompress: %w", err)
		}
	}

	// 3. Prepare the backup into a consistent state.
	prepareArgs, err := xtrabackup.PrepareArgs(opts.BackupDir)
	if err != nil {
		return err
	}
	log.Info("Preparing backup")
	if err := runCommand(ctx, opts.XtrabackupPath, prepareArgs); err != nil {
		return fmt.Errorf("xtrabackup prepare: %w", err)
	}

	// 4. Restore into the (empty) data directory.
	if err := os.MkdirAll(opts.DataDir, 0o750); err != nil {
		return fmt.Errorf("creating data dir: %w", err)
	}
	copyBackArgs, err := xtrabackup.CopyBackArgs(opts.BackupDir, opts.DataDir)
	if err != nil {
		return err
	}
	log.Info("Restoring backup")
	if err := runCommand(ctx, opts.XtrabackupPath, copyBackArgs); err != nil {
		return fmt.Errorf("xtrabackup copy-back: %w", err)
	}

	log.Info("Completed restore from object store")
	return nil
}

// extract downloads the archive and pipes it into `xbstream -x`, returning the
// SHA256 of the downloaded bytes.
func (o *RestoreOptions) extract(ctx context.Context) (string, error) {
	log := logf.FromContext(ctx).WithName("instance-restore")
	extractArgs, err := xtrabackup.ExtractArgs(o.BackupDir)
	if err != nil {
		return "", err
	}

	pipeReader, pipeWriter := io.Pipe()
	hasher := objectstore.NewSHA256Writer(pipeWriter)

	extract := exec.CommandContext(ctx, o.XBStreamPath, extractArgs...)
	extract.Stdin = pipeReader
	extractOut, extractErr := newProcessLogWriters(log.WithName("xbstream"))
	extract.Stdout = extractOut
	extract.Stderr = extractErr
	log.Info("Extracting backup stream", "binary", o.XBStreamPath)
	if err := extract.Start(); err != nil {
		_ = pipeReader.CloseWithError(err)
		return "", fmt.Errorf("starting xbstream: %w", err)
	}

	// Download into the pipe; closing it signals EOF to xbstream.
	downloadErr := make(chan error, 1)
	go func() {
		_, err := o.Store.Download(ctx, o.Bucket, o.ArchiveKey, hasher)
		// Surface any download error to the reader so xbstream fails too.
		_ = pipeWriter.CloseWithError(err)
		downloadErr <- err
	}()

	waitErr := extract.Wait()
	dlErr := <-downloadErr
	if dlErr != nil {
		return "", dlErr
	}
	if waitErr != nil {
		return "", fmt.Errorf("xbstream extract: %w", waitErr)
	}
	return hasher.SumHex(), nil
}
