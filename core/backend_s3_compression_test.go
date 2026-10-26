package core

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/yandex-cloud/geesefs/core/cfg"
)

func TestS3ListingCompressionPreservesObjectBytes(t *testing.T) {
	var object bytes.Buffer
	zw := gzip.NewWriter(&object)
	if _, err := io.WriteString(zw, "stored gzip payload, not HTTP transfer compression"); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"v1", "v2", "ext", "ext-fallback"} {
		t.Run(mode, func(t *testing.T) {
			var mu sync.Mutex
			var listRequests, objectRequests int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.URL.Path == "/testbucket" {
					listRequests++
					if got := r.Header.Get("Accept-Encoding"); got != "gzip" {
						t.Errorf("LIST Accept-Encoding = %q, want gzip", got)
					}
					if mode == "ext-fallback" && r.URL.Query().Get("list-type") == "ext-v1" {
						w.WriteHeader(http.StatusNotImplemented)
						fmt.Fprint(w, `<Error><Code>NotImplemented</Code><Message>use standard LIST</Message></Error>`)
						return
					}
					w.Header().Set("Content-Encoding", "gzip")
					writer := gzip.NewWriter(w)
					fmt.Fprint(writer, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>object.gz</Key><Size>42</Size><ETag>&quot;etag&quot;</ETag></Contents></ListBucketResult>`)
					if err := writer.Close(); err != nil {
						t.Error(err)
					}
					return
				}
				objectRequests++
				if got := r.Header.Get("Accept-Encoding"); got != "identity" {
					t.Errorf("object Accept-Encoding = %q, want identity", got)
				}
				w.Header().Set("Content-Encoding", "gzip")
				w.Header().Set("ETag", `"payload"`)
				body := object.Bytes()
				if r.Header.Get("Range") != "" {
					if got := r.Header.Get("Range"); got != "bytes=1-9" {
						t.Errorf("Range = %q, want bytes=1-9", got)
					}
					w.Header().Set("Content-Range", fmt.Sprintf("bytes 1-9/%d", len(body)))
					body = body[1:10]
					w.Header().Set("Content-Length", fmt.Sprint(len(body)))
					w.WriteHeader(http.StatusPartialContent)
				} else {
					w.Header().Set("Content-Length", fmt.Sprint(len(body)))
				}
				_, _ = w.Write(body)
			}))
			defer srv.Close()
			s, err := NewS3("testbucket", &cfg.FlagStorage{Endpoint: srv.URL, HTTPTimeout: 5 * time.Second}, (&cfg.S3Config{
				Region: "us-east-1", AccessKey: "test", SecretKey: "test",
				ListV2: mode != "v1", ListV1Ext: mode == "ext" || mode == "ext-fallback",
			}).Init())
			if err != nil {
				t.Fatal(err)
			}
			out, err := s.ListBlobs(&ListBlobsInput{})
			if err != nil || len(out.Items) != 1 || *out.Items[0].Key != "object.gz" || out.Items[0].Size != 42 {
				t.Fatalf("compressed listing = %#v, %v", out, err)
			}
			for _, in := range []*GetBlobInput{{Key: "object.gz"}, {Key: "object.gz", Start: 1, Count: 9}} {
				blob, err := s.GetBlob(in)
				if err != nil {
					t.Fatal(err)
				}
				body, readErr := io.ReadAll(blob.Body)
				closeErr := blob.Body.Close()
				want := object.Bytes()
				if in.Count != 0 {
					want = want[1:10]
				}
				if readErr != nil || closeErr != nil || !bytes.Equal(body, want) || blob.Size != uint64(len(want)) {
					t.Fatalf("object bytes/size changed: got %x, size %d, errors %v / %v; want %x", body, blob.Size, readErr, closeErr, want)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			wantLists := 1
			if mode == "ext-fallback" {
				wantLists = 2
			}
			if listRequests != wantLists || objectRequests != 2 {
				t.Fatalf("LIST/object requests = %d/%d, want %d/2", listRequests, objectRequests, wantLists)
			}
		})
	}
}
