package core

import (
	"bytes"
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/yandex-cloud/geesefs/core/cfg"
)

type failingSyncBackend struct {
	TestBackend
	putError  error
	copyError error
}

func (b *failingSyncBackend) PutBlob(p *PutBlobInput) (*PutBlobOutput, error) {
	return nil, b.putError
}

func (b *failingSyncBackend) CopyBlob(p *CopyBlobInput) (*CopyBlobOutput, error) {
	return nil, b.copyError
}

func syncFailureFS(t *testing.T, backend *failingSyncBackend) *Goofys {
	t.Helper()
	flags := cfg.DefaultFlags()
	flags.MaxFlushers = 0 // SyncFile starts the real flush; no uncontrolled background retry.
	flags.NoPreloadDir = true
	flags.Cheap = true
	flags.EnableMtime = true
	fs, err := newGoofys(context.Background(), "test", flags, func(string, *cfg.FlagStorage) (StorageBackend, error) {
		return backend, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fs.Shutdown)
	return fs
}

func TestSyncTreeReportsUploadFailure(t *testing.T) {
	backend := &failingSyncBackend{TestBackend: TestBackend{err: syscall.ENOENT}, putError: syscall.EACCES}
	fs := syncFailureFS(t, backend)
	root, err := fs.LookupPath("")
	if err != nil {
		t.Fatal(err)
	}
	inode, handle, err := root.Create("package.apk")
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.WriteFile(0, []byte("package payload"), true); err != nil {
		handle.Release()
		t.Fatal(err)
	}
	handle.Release()
	if err := fs.SyncTree(root); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("directory fsync = %v, want upload EACCES", err)
	}
	// The failing payload must remain dirty rather than being silently discarded.
	data, _, err := mustOpenSyncTest(t, inode).ReadFile(0, 15)
	if err != nil || string(bytes.Join(data, nil)) != "package payload" {
		t.Fatalf("failed upload lost buffered payload: data=%q err=%v", data, err)
	}
}

func mustOpenSyncTest(t *testing.T, inode *Inode) *FileHandle {
	t.Helper()
	handle, err := inode.OpenFile()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(handle.Release)
	return handle
}

func TestSyncFileReportsMissingMetadataCopySource(t *testing.T) {
	backend := &failingSyncBackend{TestBackend: TestBackend{err: syscall.ENOENT}, copyError: syscall.ENOENT}
	backend.HeadBlobFunc = func(p *HeadBlobInput) (*HeadBlobOutput, error) {
		if p.Key != "package.apk" {
			return nil, syscall.ENOENT
		}
		return &HeadBlobOutput{BlobItemOutput: BlobItemOutput{
			Key: PString(p.Key), Size: 15, ETag: PString("existing-etag"), LastModified: PTime(time.Unix(1, 0)),
		}}, nil
	}
	fs := syncFailureFS(t, backend)
	inode, err := fs.LookupPath("package.apk")
	if err != nil {
		t.Fatal(err)
	}
	mtime := time.Unix(2, 0)
	if err := inode.SetAttributes(nil, nil, &mtime, nil, nil); err != nil {
		t.Fatal(err)
	}
	// A metadata-only self-copy now receives NoSuchKey from the real flush path.
	// Discarding the local cache must not turn this failed persistence into success.
	if err := inode.SyncFile(); !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("file fsync = %v, want metadata-copy ENOENT", err)
	}
}

func TestSyncFileReportsMissingRenameSource(t *testing.T) {
	backend := &failingSyncBackend{TestBackend: TestBackend{err: syscall.ENOENT}, copyError: syscall.ENOENT}
	backend.HeadBlobFunc = func(p *HeadBlobInput) (*HeadBlobOutput, error) {
		if p.Key != "temporary.apk" {
			return nil, syscall.ENOENT
		}
		return &HeadBlobOutput{BlobItemOutput: BlobItemOutput{
			Key: PString(p.Key), Size: 15, ETag: PString("existing-etag"), LastModified: PTime(time.Unix(1, 0)),
		}}, nil
	}
	fs := syncFailureFS(t, backend)
	root, err := fs.LookupPath("")
	if err != nil {
		t.Fatal(err)
	}
	inode, err := fs.LookupPath("temporary.apk")
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Rename("temporary.apk", root, "package.apk"); err != nil {
		t.Fatal(err)
	}
	if err := inode.SyncFile(); !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("renamed file fsync = %v, want copy-source ENOENT", err)
	}
}
