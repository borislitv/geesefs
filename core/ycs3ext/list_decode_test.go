package ycs3ext

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	awsclient "github.com/aws/aws-sdk-go/aws/client"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/private/protocol/restxml"
	"github.com/aws/aws-sdk-go/service/s3"
)

const listXML = `<?xml version="1.0"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Name>bucket</Name><Prefix>pkgs/</Prefix><Delimiter>/</Delimiter>
  <EncodingType>url</EncodingType><MaxKeys>1000</MaxKeys><KeyCount>2</KeyCount>
  <ContinuationToken>previous&amp;token</ContinuationToken><StartAfter>pkgs/a</StartAfter>
  <IsTruncated>true</IsTruncated><NextContinuationToken>next&amp;token</NextContinuationToken>
  <Contents><Key>pkgs/a%20b&amp;c</Key><ETag>&quot;etag&quot;</ETag>
    <LastModified>2026-10-25T12:34:56.123Z</LastModified><Size>42</Size>
    <StorageClass>STANDARD</StorageClass><Owner><ID>owner</ID><DisplayName>name</DisplayName></Owner>
    <Metadata><Name>mtime</Name><Value>123.456</Value></Metadata>
    <Metadata><Name>empty</Name><Value></Value></Metadata>
    <Metadata><Name>mtime</Name><Value>789</Value></Metadata>
    <Future><Contents><Key>not-an-object</Key></Contents></Future>
  </Contents>
  <Contents><Key>pkgs/empty</Key><ETag/><Size>0</Size></Contents>
  <CommonPrefixes><Prefix>pkgs/sub/</Prefix><Future/></CommonPrefixes>
  <Future><IsTruncated>false</IsTruncated></Future>
</ListBucketResult>`

type listTransport func(*http.Request) (*http.Response, error)

func (fn listTransport) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func listClient(client *http.Client, endpoint string) *S3Client {
	return NewS3Client(s3.New(session.Must(session.NewSession(&aws.Config{
		Region: aws.String("us-east-1"), Endpoint: aws.String(endpoint),
		Credentials: credentials.AnonymousCredentials, S3ForcePathStyle: aws.Bool(true),
		MaxRetries: aws.Int(0), HTTPClient: client,
	}))))
}

func TestListExtXMLFields(t *testing.T) {
	client := listClient(&http.Client{Transport: listTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"X-Amz-Request-Id": {"list-id"}},
			Body: io.NopCloser(strings.NewReader(listXML))}, nil
	})}, "http://s3.invalid")
	out, req, err := client.ListObjectsV1Ext(&ListObjectsV1ExtInput{Bucket: aws.String("bucket")})
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2026, 10, 25, 12, 34, 56, 123000000, time.UTC)
	want := &ListObjectsV1ExtOutput{
		Name: aws.String("bucket"), Prefix: aws.String("pkgs/"), Delimiter: aws.String("/"),
		EncodingType: aws.String("url"), MaxKeys: aws.Int64(1000), KeyCount: aws.Int64(2),
		ContinuationToken: aws.String("previous&token"), StartAfter: aws.String("pkgs/a"),
		IsTruncated: aws.Bool(true), NextContinuationToken: aws.String("next&token"),
		Contents: []*ListObjectExt{
			{Key: aws.String("pkgs/a%20b&c"), ETag: aws.String(`"etag"`), LastModified: &stamp,
				Size: aws.Int64(42), StorageClass: aws.String("STANDARD"),
				Owner:        &s3.Owner{ID: aws.String("owner"), DisplayName: aws.String("name")},
				UserMetadata: map[string]*string{"mtime": aws.String("789"), "empty": aws.String("")}},
			{Key: aws.String("pkgs/empty"), ETag: aws.String(""), Size: aws.Int64(0)},
		},
		CommonPrefixes: []*s3.CommonPrefix{{Prefix: aws.String("pkgs/sub/")}},
	}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("decoded listing differs: got %#v, want %#v", out, want)
	}
	if req.RequestID != "list-id" {
		t.Fatalf("request ID = %q, want list-id", req.RequestID)
	}
}

func TestListExtResponseIntegrity(t *testing.T) {
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := io.WriteString(writer, listXML); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	badCRC := bytes.Clone(compressed.Bytes())
	badCRC[len(badCRC)-8] ^= 1
	for _, tc := range []struct {
		name, encoding string
		body           []byte
		wantError      bool
	}{
		{"plain", "", []byte(listXML), false},
		{"gzip", "gzip", compressed.Bytes(), false},
		{"gzip-checksum", "gzip", badCRC, true},
		{"gzip-trailer-truncated", "gzip", compressed.Bytes()[:compressed.Len()-5], true},
		{"truncated-xml", "", []byte(`<ListBucketResult><Contents><Key>a</Key>`), true},
		{"bad-size", "", []byte(`<ListBucketResult><Contents><Size>NaN</Size></Contents></ListBucketResult>`), true},
		{"bad-time", "", []byte(`<ListBucketResult><Contents><LastModified>bad</LastModified></Contents></ListBucketResult>`), true},
		{"bad-bool", "", []byte(`<ListBucketResult><IsTruncated>bad</IsTruncated></ListBucketResult>`), true},
		{"trailing-xml", "", []byte(listXML + `<`), true},
		{"second-document", "", []byte(listXML + `<ListBucketResult/>`), true},
		{"missing-metadata-value", "", []byte(`<ListBucketResult><Contents><Metadata><Name>mtime</Name></Metadata></Contents></ListBucketResult>`), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.encoding != "" {
					if r.Header.Get("Accept-Encoding") != "gzip" {
						t.Error("listing did not negotiate gzip")
					}
					w.Header().Set("Content-Encoding", tc.encoding)
				}
				_, _ = w.Write(tc.body)
			}))
			defer srv.Close()
			transport := &http.Transport{Proxy: nil}
			defer transport.CloseIdleConnections()
			client := listClient(&http.Client{Transport: transport, Timeout: 5 * time.Second}, srv.URL)
			out, _, err := client.ListObjectsV1Ext(&ListObjectsV1ExtInput{Bucket: aws.String("bucket")})
			if tc.wantError {
				if err == nil {
					t.Fatal("invalid response accepted as a successful listing")
				}
				if len(out.Contents) != 0 || out.NextContinuationToken != nil {
					t.Fatal("failed response published partial output")
				}
			} else if err != nil || len(out.Contents) != 2 || aws.StringValue(out.NextContinuationToken) != "next&token" {
				t.Fatalf("valid response: output=%#v, err=%v", out, err)
			}
		})
	}
}

func TestListExtServiceError(t *testing.T) {
	client := listClient(&http.Client{Transport: listTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 403, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(`<Error><Code>AccessDenied</Code><Message>denied</Message></Error>`))}, nil
	})}, "http://s3.invalid")
	_, _, err := client.ListObjectsV1Ext(&ListObjectsV1ExtInput{Bucket: aws.String("bucket")})
	awsErr, ok := err.(awserr.Error)
	if !ok || awsErr.Code() != "AccessDenied" {
		t.Fatalf("service error = %v, want AccessDenied", err)
	}
}

type trackedListBody struct {
	io.Reader
	closed bool
}

func (b *trackedListBody) Close() error { b.closed = true; return nil }

type interruptedList struct{}

func (interruptedList) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestListExtFragmentationRetryAndOwnership(t *testing.T) {
	var bodies []*trackedListBody
	client := listClient(&http.Client{Transport: listTransport(func(r *http.Request) (*http.Response, error) {
		body := &trackedListBody{}
		switch len(bodies) {
		case 0:
			body.Reader = io.MultiReader(strings.NewReader(`<ListBucketResult><Contents><Key>partial</Key></Contents>`), interruptedList{})
		case 1:
			body.Reader = iotest.OneByteReader(strings.NewReader(`<ListBucketResult><Contents><Key>пакет&amp;版本<![CDATA[.rpm]]></Key><Size>7</Size></Contents></ListBucketResult>`))
		default:
			body.Reader = strings.NewReader(`<ListBucketResult><Contents><Key>different</Key></Contents></ListBucketResult>`)
		}
		bodies = append(bodies, body)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}, nil
	})}, "http://s3.invalid")
	client.Retryer = awsclient.DefaultRetryer{NumMaxRetries: 1, MinRetryDelay: time.Nanosecond, MaxRetryDelay: time.Nanosecond}
	first, req, err := client.ListObjectsV1Ext(&ListObjectsV1ExtInput{Bucket: aws.String("bucket")})
	if err != nil || req.RetryCount != 1 || len(first.Contents) != 1 || aws.StringValue(first.Contents[0].Key) != "пакет&版本.rpm" || aws.Int64Value(first.Contents[0].Size) != 7 {
		t.Fatalf("fragmented/retried listing = %#v, retry count %d, err=%v", first, req.RetryCount, err)
	}
	second, _, err := client.ListObjectsV1Ext(&ListObjectsV1ExtInput{Bucket: aws.String("bucket")})
	if err != nil || len(second.Contents) != 1 || aws.StringValue(second.Contents[0].Key) != "different" {
		t.Fatalf("second listing = %#v, err=%v", second, err)
	}
	if aws.StringValue(first.Contents[0].Key) != "пакет&版本.rpm" {
		t.Fatal("a later response overwrote previously returned data")
	}
	for i, body := range bodies {
		if !body.closed {
			t.Errorf("response body %d was not closed", i)
		}
	}
}

func BenchmarkListExtDecode(b *testing.B) {
	var page strings.Builder
	page.WriteString(`<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>next</NextContinuationToken>`)
	for i := 0; i < 1000; i++ {
		fmt.Fprintf(&page, `<Contents><Key>packages/%06d.rpm</Key><ETag>&quot;0123456789abcdef&quot;</ETag><Size>1048576</Size><LastModified>2026-10-25T12:34:56.123Z</LastModified><StorageClass>STANDARD</StorageClass><Metadata><Name>mtime</Name><Value>1234567890</Value></Metadata></Contents>`, i)
	}
	page.WriteString(`</ListBucketResult>`)
	body := page.String()
	client := listClient(&http.Client{Transport: listTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}, "http://s3.invalid")
	for _, decoder := range []string{"sdk-tree", "stream"} {
		b.Run(decoder, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			for i := 0; i < b.N; i++ {
				req, out := client.ListObjectsV1ExtRequest(&ListObjectsV1ExtInput{Bucket: aws.String("bucket")})
				if decoder == "sdk-tree" {
					// The original request-specific decoder, retained as an allocation baseline.
					req.Handlers.Unmarshal.Clear()
					req.Handlers.Unmarshal.PushBack(restxml.Unmarshal)
				}
				err := req.Send()
				if err != nil || len(out.Contents) != 1000 {
					b.Fatalf("listing = %v, err=%v", out, err)
				}
			}
		})
	}
}
