//go:build !windows

package core

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"
	. "gopkg.in/check.v1"
)

func (s *GoofysTest) TestReadOnlyDirectIOFuse(t *C) {
	s.testReadOnlyDataCacheFuse(t, true)
}

func (s *GoofysTest) TestReadOnlyKernelCacheFuse(t *C) {
	s.testReadOnlyDataCacheFuse(t, false)
}

func (s *GoofysTest) testReadOnlyDataCacheFuse(t *C, direct bool) {
	data := bytes.Repeat([]byte("unaligned package payload\x00\xff"), 8193)
	for name, body := range map[string][]byte{"direct-data": data, "direct-empty": nil} {
		_, err := s.cloud.PutBlob(&PutBlobInput{Key: name, Body: bytes.NewReader(body), Size: PUInt64(uint64(len(body)))})
		t.Assert(err, IsNil)
	}
	s.fs.flags.ReadOnlyDirectIO = direct
	s.fs.flags.MountOptions = []string{"ro"}
	mountPoint := filepath.Join(s.tmp, "mnt"+s.fs.bucket)
	s.mountInside(t, mountPoint)
	defer s.umount(t, mountPoint)

	path := filepath.Join(mountPoint, "direct-data")
	f, err := os.Open(path)
	t.Assert(err, IsNil)
	defer f.Close()
	got, err := io.ReadAll(f)
	t.Assert(err, IsNil)
	t.Assert(got, DeepEquals, data)

	// ReadAt reaches the FUSE path with unaligned offsets, short tails and EOF.
	for _, off := range []int64{1, 4093, int64(len(data) - 11), int64(len(data)), int64(len(data) + 17)} {
		buf := make([]byte, 37)
		n, readErr := f.ReadAt(buf, off)
		want := 0
		if off < int64(len(data)) {
			want = len(data) - int(off)
			if want > len(buf) {
				want = len(buf)
			}
		}
		t.Assert(n, Equals, want)
		if want < len(buf) {
			t.Assert(readErr, Equals, io.EOF)
		} else {
			t.Assert(readErr, IsNil)
		}
		if want > 0 {
			t.Assert(buf[:n], DeepEquals, data[off:off+int64(want)])
		}
	}

	results := make(chan error, 8)
	for i := 0; i < cap(results); i++ {
		go func(off int64) {
			buf := make([]byte, 113)
			n, err := f.ReadAt(buf, off)
			if err == nil && (n != len(buf) || !bytes.Equal(buf, data[off:off+113])) {
				err = fmt.Errorf("concurrent ReadAt(%d) returned incorrect data", off)
			}
			results <- err
		}(int64(i*131 + 7))
	}
	for i := 0; i < cap(results); i++ {
		t.Assert(<-results, IsNil)
	}

	if runtime.GOOS == "linux" {
		for _, mode := range []int{unix.MAP_PRIVATE, unix.MAP_SHARED} {
			view, mmapErr := unix.Mmap(int(f.Fd()), 0, len(data), unix.PROT_READ, mode)
			if mmapErr == nil {
				matches := bytes.Equal(view, data)
				t.Assert(unix.Munmap(view), IsNil)
				t.Assert(matches, Equals, true)
			}
			if direct && mode == unix.MAP_SHARED {
				t.Assert(mmapErr, Equals, syscall.ENODEV)
			} else {
				t.Assert(mmapErr, IsNil)
			}
		}
	}
	got, err = os.ReadFile(path)
	t.Assert(err, IsNil)
	t.Assert(got, DeepEquals, data)
	got, err = os.ReadFile(filepath.Join(mountPoint, "direct-empty"))
	t.Assert(err, IsNil)
	t.Assert(len(got), Equals, 0)
	writable, err := os.OpenFile(path, os.O_WRONLY, 0)
	if writable != nil {
		writable.Close()
	}
	t.Assert(errors.Is(err, syscall.EROFS), Equals, true)
}
