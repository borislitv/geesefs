package core

import (
	"testing"
	"time"

	"github.com/yandex-cloud/geesefs/core/cfg"
)

func TestSlurpRefreshExternalChange(t *testing.T) {
	for _, scope := range []string{"file", "directory", "root"} {
		t.Run(scope, func(t *testing.T) {
			fs, store := lookupFS(t, time.Minute, func(flags *cfg.FlagStorage) {
				flags.NoPreloadDir = false
				flags.Cheap = false
				flags.NegativeLookupCacheSize = 0
			})
			store.put("dir/file")
			file, err := fs.LookupPath("dir/file")
			if err != nil {
				t.Fatal(err)
			}
			if file.Attributes.Size != 3 {
				t.Fatalf("initial size = %d, want 3", file.Attributes.Size)
			}
			store.mu.Lock()
			store.objects["dir/file"] = BlobItemOutput{
				Key: PString("dir/file"), Size: 4, LastModified: PTime(time.Now()),
			}
			store.mu.Unlock()
			target := file
			if scope == "directory" {
				target, err = fs.LookupPath("dir")
			} else if scope == "root" {
				target, err = fs.LookupPath("")
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := fs.RefreshInodeCache(target); err != nil {
				t.Fatal(err)
			}
			refreshed, err := fs.LookupPath("dir/file")
			if err != nil {
				t.Fatal(err)
			}
			if refreshed.Attributes.Size != 4 {
				t.Fatalf("size after %s refresh = %d, want external size 4", scope, refreshed.Attributes.Size)
			}
		})
	}
}

func TestSlurpRefreshExternalCreation(t *testing.T) {
	fs, store := lookupFS(t, time.Minute, func(flags *cfg.FlagStorage) {
		flags.NoPreloadDir = false
		flags.Cheap = false
		flags.NegativeLookupCacheSize = 0
	})
	store.put("dir/file")
	if _, err := fs.LookupPath("dir/file"); err != nil {
		t.Fatal(err)
	}
	requireMissing(t, fs, "dir/new")
	store.put("dir/new")
	dir, err := fs.LookupPath("dir")
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.RefreshInodeCache(dir); err != nil {
		t.Fatal(err)
	}
	created, err := fs.LookupPath("dir/new")
	if err != nil || created == nil || created.Attributes.Size != 3 {
		t.Fatalf("external creation hidden after directory refresh: %v, %v", created, err)
	}
}
