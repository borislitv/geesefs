package core

import (
	"bytes"
	"io"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/yandex-cloud/geesefs/core/cfg"
)

func TestOwnedReadBufferAccounting(t *testing.T) {
	for _, tc := range []struct {
		name        string
		dirtyOffset uint64
		dirtySize   int
		copyData    bool
	}{
		{"prefix", 0, 100, false},
		{"middle", 50, 100, false},
		{"covered", 0, 256, false},
		{"copied-middle", 50, 100, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := BufferList{helpers: &TestBLHelpers{}}
			dirty := bytes.Repeat([]byte{0xd1}, tc.dirtySize)
			used := l.Add(tc.dirtyOffset, dirty, BUF_DIRTY, false)
			payload := bytes.Repeat([]byte{0xc2}, 256)
			used += l.Add(0, payload, BUF_CLEAN, tc.copyData)
			wantUsed := int64(tc.dirtySize)
			if tc.dirtySize < len(payload) {
				if tc.copyData {
					wantUsed += int64(len(payload) - tc.dirtySize)
				} else {
					// Even one retained slice keeps the complete owned array alive.
					wantUsed += int64(len(payload))
				}
			}
			if used != wantUsed {
				t.Errorf("retained memory = %d, want %d", used, wantUsed)
			}
			data, _, err := l.GetData(0, 256, false)
			want := bytes.Repeat([]byte{0xc2}, 256)
			copy(want[tc.dirtyOffset:], dirty)
			if err != nil || !bytes.Equal(bytes.Join(data, nil), want) {
				t.Fatalf("clean download overwrote dirty data: %x, %v", data, err)
			}
			used += l.RemoveRange(0, 256, nil)
			if used != 0 {
				t.Errorf("memory after removing all buffers = %d, want 0", used)
			}
			// A caller can retain the slices beyond cache eviction. They must not
			// be recycled or modified when the cache relinquishes ownership.
			l.Add(0, bytes.Repeat([]byte{0xe3}, 256), BUF_CLEAN, false)
			if !bytes.Equal(bytes.Join(data, nil), want) {
				t.Fatal("evicted read data changed after another cache insertion")
			}
		})
	}
}

type readBodyBackend struct {
	StorageBackend
	get func(*GetBlobInput) (*GetBlobOutput, error)
}

func (b *readBodyBackend) GetBlob(in *GetBlobInput) (*GetBlobOutput, error) {
	return b.get(in)
}

type observedReadBody struct {
	io.Reader
	closed chan struct{}
	once   sync.Once
}

func (b *observedReadBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

type failingPayloadReader struct{}

func (failingPayloadReader) Read([]byte) (int, error) { return 0, syscall.EACCES }

func TestReadFileClosesResponseBody(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "read-error"
		}
		t.Run(name, func(t *testing.T) {
			fs, store := lookupFS(t, time.Minute, func(f *cfg.FlagStorage) {
				f.GCInterval = 0
				f.ReadRetryAttempts = 1
			})
			store.put("payload")
			root, err := fs.LookupPath("")
			if err != nil {
				t.Fatal(err)
			}
			body := &observedReadBody{Reader: bytes.NewReader([]byte("raw")), closed: make(chan struct{})}
			if fail {
				body.Reader = failingPayloadReader{}
			}
			root.dir.cloud = &readBodyBackend{StorageBackend: root.dir.cloud, get: func(in *GetBlobInput) (*GetBlobOutput, error) {
				return &GetBlobOutput{Body: body}, nil
			}}
			inode, err := fs.LookupPath("payload")
			if err != nil {
				t.Fatal(err)
			}
			fh, err := inode.OpenFile()
			if err != nil {
				t.Fatal(err)
			}
			defer fh.Release()
			data, n, err := fh.ReadFile(0, 3)
			if fail {
				if err != syscall.EACCES || n != 0 {
					t.Fatalf("failed read = %x, %d, %v; want EACCES", data, n, err)
				}
			} else if err != nil || n != 3 || !bytes.Equal(bytes.Join(data, nil), []byte("raw")) {
				t.Fatalf("read = %x, %d, %v; want raw", data, n, err)
			}
			select {
			case <-body.closed:
			case <-time.After(2 * time.Second):
				t.Fatal("GET response body was not closed after ReadFile completed")
			}
		})
	}
}
