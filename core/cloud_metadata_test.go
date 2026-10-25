package core

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jacobsa/fuse/fuseops"
	"github.com/yandex-cloud/geesefs/core/cfg"
)

func TestCloudXattrsFromListing(t *testing.T) {
	fs, store := lookupFS(t, time.Minute)
	store.put("package.rpm")
	inode, err := fs.LookupPath("package.rpm")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name        string
		etag, class *string
		wantNames   []string
	}{
		{"present", PString("\"abc123\""), PString("STANDARD"), []string{"s3.etag", "s3.storage-class"}},
		{"updated", PString("\"def456\""), PString("COLD"), []string{"s3.etag", "s3.storage-class"}},
		{"empty-but-present", PString(""), PString(""), []string{"s3.etag", "s3.storage-class"}},
		{"removed", nil, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inode.SetFromBlobItem(&BlobItemOutput{Size: 3, ETag: tc.etag, StorageClass: tc.class,
				Metadata: map[string]*string{}})
			before := store.count()
			for name, want := range map[string]*string{"s3.etag": tc.etag, "s3.storage-class": tc.class} {
				got, err := inode.GetXattr(name)
				if want == nil {
					if err != ENOATTR {
						t.Fatalf("GetXattr(%q) = %q, %v; want ENOATTR", name, got, err)
					}
				} else if err != nil || string(got) != *want {
					t.Fatalf("GetXattr(%q) = %q, %v; want %q", name, got, err, *want)
				}
			}
			names, err := inode.ListXattr()
			if err != nil || !reflect.DeepEqual(names, tc.wantNames) {
				t.Fatalf("ListXattr = %v, %v; want %v", names, err, tc.wantNames)
			}
			if store.count() != before {
				t.Fatal("cached listing attributes caused a backend request")
			}
		})
	}
}

func TestCloudXattrsHeadAndUserNamespace(t *testing.T) {
	fs, store := lookupFS(t, time.Minute)
	store.objects["package.rpm"] = BlobItemOutput{Key: PString("package.rpm"), Size: 3,
		ETag: PString("\"head-etag\"")}
	inode, err := fs.LookupPath("package.rpm")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inode.GetXattr("s3.unknown"); err != ENOATTR {
		t.Fatalf("unknown cloud attribute = %v; want ENOATTR", err)
	}
	// The HEAD result has no class: filling xattrs must retain the STANDARD default.
	if _, err := inode.GetXattr("user.absent"); err != ENOATTR {
		t.Fatalf("absent user attribute = %v; want ENOATTR", err)
	}
	got, err := inode.GetXattr("s3.storage-class")
	if err != nil || string(got) != "STANDARD" {
		t.Fatalf("HEAD storage class = %q, %v; want STANDARD", got, err)
	}
	if err := inode.SetXattr("user.comment", []byte("kept"), 0); err != nil {
		t.Fatal(err)
	}
	// Cloud attributes remain read-only, including writes and removal.
	if err := inode.SetXattr("s3.etag", []byte("overwritten"), 0); err != nil {
		t.Fatal(err)
	}
	if err := inode.RemoveXattr("s3.etag"); err != nil {
		t.Fatal(err)
	}
	got, err = inode.GetXattr("s3.etag")
	if err != nil || string(got) != "\"head-etag\"" {
		t.Fatalf("read-only ETag = %q, %v", got, err)
	}
	names, err := inode.ListXattr()
	want := []string{"s3.etag", "s3.storage-class", "user.comment"}
	if err != nil || !reflect.DeepEqual(names, want) {
		t.Fatalf("ListXattr = %v, %v; want %v", names, err, want)
	}
	if err := inode.RemoveXattr("user.comment"); err != nil {
		t.Fatal(err)
	}
	if _, err := inode.GetXattr("user.comment"); err != ENOATTR {
		t.Fatalf("removed user attribute = %v; want ENOATTR", err)
	}
}

func TestCloudXattrsConcurrentRefresh(t *testing.T) {
	fs, store := lookupFS(t, time.Minute)
	store.put("package.rpm")
	inode, err := fs.LookupPath("package.rpm")
	if err != nil {
		t.Fatal(err)
	}
	inode.SetFromBlobItem(&BlobItemOutput{Size: 3, ETag: PString("etag"), StorageClass: PString("STANDARD")})
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				inode.SetFromBlobItem(&BlobItemOutput{Size: 3, ETag: PString("etag"), StorageClass: PString("STANDARD")})
				got, err := inode.GetXattr("s3.etag")
				if err != nil || string(got) != "etag" {
					t.Errorf("concurrent ETag = %q, %v", got, err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

var cloudMetadataInodeSink *Inode

func BenchmarkListedInodeCloudMetadata(b *testing.B) {
	fs := &Goofys{flags: cfg.DefaultFlags(), bufferPool: NewBufferPool(32<<20, 0),
		inodesByTime: make(map[int64]map[fuseops.InodeID]bool)}
	item := &BlobItemOutput{Size: 1024, ETag: PString("\"0123456789abcdef0123456789abcdef\""),
		StorageClass: PString("STANDARD"), Metadata: map[string]*string{}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		inode := NewInode(fs, nil, "package.rpm")
		inode.SetFromBlobItem(item)
		cloudMetadataInodeSink = inode
	}
}

func BenchmarkRefreshInodeCloudMetadata(b *testing.B) {
	fs := &Goofys{flags: cfg.DefaultFlags(), bufferPool: NewBufferPool(32<<20, 0),
		inodesByTime: make(map[int64]map[fuseops.InodeID]bool)}
	inode := NewInode(fs, nil, "package.rpm")
	item := &BlobItemOutput{Size: 1024, ETag: PString("\"0123456789abcdef0123456789abcdef\""),
		StorageClass: PString("STANDARD"), Metadata: map[string]*string{}}
	inode.SetFromBlobItem(item)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		inode.SetFromBlobItem(item)
	}
	cloudMetadataInodeSink = inode
}

func BenchmarkRetainedCloudMetadata(b *testing.B) {
	for _, size := range []int{1024, 16384} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			fs := &Goofys{flags: cfg.DefaultFlags(), bufferPool: NewBufferPool(32<<20, 0),
				inodesByTime: make(map[int64]map[fuseops.InodeID]bool)}
			inodes := make([]*Inode, size)
			item := &BlobItemOutput{Size: 1024, ETag: PString("\"0123456789abcdef0123456789abcdef\""),
				StorageClass: PString("STANDARD"), Metadata: map[string]*string{}}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for j := range inodes {
					inode := NewInode(fs, nil, "package.rpm")
					inode.SetFromBlobItem(item)
					inodes[j] = inode
				}
			}
			cloudMetadataInodeSink = inodes[len(inodes)-1]
		})
	}
}
