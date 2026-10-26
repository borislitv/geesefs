//go:build !windows

package core

import (
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/jacobsa/fuse/fuseops"
	"github.com/yandex-cloud/geesefs/core/cfg"
)

func TestReadOnlyDirectIOOpen(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		op      fuseops.OpenFileOp
		want    bool
	}{
		{name: "default-read", op: fuseops.OpenFileOp{OpenFlags: syscall.O_RDONLY}},
		{name: "default-write", op: fuseops.OpenFileOp{OpenFlags: syscall.O_WRONLY}},
		{name: "default-read-write", op: fuseops.OpenFileOp{OpenFlags: syscall.O_RDWR}},
		{name: "opt-in-read", enabled: true, op: fuseops.OpenFileOp{OpenFlags: syscall.O_RDONLY}, want: true},
		{name: "opt-in-read-extra-flags", enabled: true, op: fuseops.OpenFileOp{OpenFlags: syscall.O_RDONLY | syscall.O_NONBLOCK}, want: true},
		{name: "opt-in-write", enabled: true, op: fuseops.OpenFileOp{OpenFlags: syscall.O_WRONLY}},
		{name: "opt-in-read-write", enabled: true, op: fuseops.OpenFileOp{OpenFlags: syscall.O_RDWR}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs, store := lookupFS(t, time.Minute, func(flags *cfg.FlagStorage) {
				flags.ReadOnlyDirectIO = tc.enabled
			})
			store.put("package.bin")
			inode, err := fs.LookupPath("package.bin")
			if err != nil {
				t.Fatal(err)
			}
			server := NewGoofysFuse(fs)
			op := tc.op
			op.Inode = inode.Id
			if err := server.OpenFile(context.Background(), &op); err != nil {
				t.Fatal(err)
			}
			defer server.ReleaseFileHandle(context.Background(), &fuseops.ReleaseFileHandleOp{Handle: op.Handle})
			if op.UseDirectIO != tc.want || op.KeepPageCache {
				t.Fatalf("open flags: direct I/O=%v keep page cache=%v; want %v, false", op.UseDirectIO, op.KeepPageCache, tc.want)
			}
		})
	}
}

func TestReadOnlyDirectIORequiresReadOnlyMount(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options []string
	}{
		{"default-writable", nil},
		{"explicit-writable", []string{"rw"}},
		{"conflicting-comma-options", []string{"ro,rw"}},
		{"conflicting-separate-options", []string{"rw", "ro"}},
		{"invalid-read-only-value", []string{"ro=false"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flags := cfg.DefaultFlags()
			flags.ReadOnlyDirectIO = true
			flags.MountOptions = tc.options
			// Invalid configurations must fail before initializing a backend or mount.
			fs, mounted, err := MountFuse(context.Background(), "test", flags)
			if err == nil || err.Error() != "--read-only-direct-io requires -o ro without rw" || fs != nil || mounted != nil {
				t.Fatalf("invalid direct I/O mount returned %v, %v, %v", fs, mounted, err)
			}
		})
	}
}
