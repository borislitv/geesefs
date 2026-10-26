# Read buffer ownership and streaming S3 listings

## File data

The existing read path already transfers each downloaded 128-KiB buffer to the
cache without copying it. `FileHandle.ReadFile` returns slices of those arrays
to FUSE. Callers can retain returned slices after cache eviction, so eviction
alone does not make a buffer safe to recycle.

Two ownership corrections apply to this path:

- `sendRead` closes each successful GET response body on both completion and
  read failure. This releases backend resources on early exits as well as the
  normal path; it does not imply that every successful HTTP read previously
  leaked a connection.
- An owned download split around concurrent dirty writes is charged for its
  entire retained source array once. Previously, insertion charged only the
  surviving slices, whereas removing the final reference subtracted the full
  array. Repeated overlaps could therefore undercount cache memory and even
  make its accounting negative. Copied fragments retain their separate
  allocation accounting, and fully covered downloads retain no array.

These are resource-lifetime and memory-budget corrections. They are not evidence
of reduced steady-state RSS on a read-only workload. A configured 32-GiB data
cache can legitimately retain approximately that much file data. The changes do
not compress or reinterpret stored package/archive bytes.

## LIST transport and decoder

S3 LIST v1, v2 and ext-v1 now permit the HTTP transport's automatic gzip
negotiation and streaming decompression. Other GET operations continue to request
`Accept-Encoding: identity`: an object's `Content-Encoding: gzip` describes its
stored bytes, which must remain byte-for-byte intact, including ranged reads.
Compression savings depend on the endpoint actually serving compressed listings.

The ext-v1 successful-response handler decodes XML directly into its final SDK
output structures. It avoids the SDK's intermediate `XMLNode` graph, per-node
maps and reflective traversal. Signing, response metadata, service errors and
retry handling continue through the SDK request pipeline.

Returned strings own their storage; decoder tokens and decompressor buffers do
not escape. A failed decode publishes no partial output. The decoder reads
through EOF after the document to check trailing XML and gzip checksum/trailer
errors before accepting a page. Unknown XML subtrees are skipped.

## Local measurements

Measured on 2026-10-26 with Go 1.25.14, Linux amd64, Intel Core i5-1340P,
`GOMAXPROCS=4`, `GOGC=20`, five 500-ms benchmark runs per decoder. Each operation
sends a complete SDK request through an in-memory transport and decodes one
synthetic page of 1,000 entries, approximately 266 kB of XML, including per-object
mtime metadata. The baseline reinstates the original SDK `restxml.Unmarshal`
handler on the same request type.

| Metric | SDK XML tree | Streaming decoder |
| --- | ---: | ---: |
| Median time per page | 19.11 ms | 11.48 ms |
| Median allocated bytes per page | 6,805,251 | 2,665,793 |
| Allocations per page | 147,184 | 80,140 |

This fixture shows approximately 60.8% fewer allocated bytes, 45.6% fewer
allocations and a 1.66x median decoding speedup. It measures decoder allocation
and CPU work, not network compression ratio, whole-reader RSS, or production
throughput.

Reproduce the comparison:

```sh
GOTOOLCHAIN=go1.25.14 GOMAXPROCS=4 GOGC=20 go test -timeout 150s ./core/ycs3ext -run '^$' -bench '^BenchmarkListExtDecode$' -benchmem -benchtime=500ms -count=5
```

## Verification

- `TestOwnedReadBufferAccounting` and `TestReadFileClosesResponseBody` fail on
  the previous implementation and pass with the ownership corrections. They
  check dirty-byte preservation, full/partial overlap, copied fragments, balanced
  accounting, retained reader slices, and body closure on success and error.
- `TestS3ListingCompressionPreservesObjectBytes` covers LIST v1, v2, ext-v1 and
  ext fallback, plus exact stored-gzip full and ranged object bytes. The fixture
  returns gzip for LIST and rejects the previous `identity` negotiation.
- Ext-v1 tests cover fields, namespaces/entities, metadata, service errors,
  fragmented Unicode/CDATA input, interrupted-response retry, independent output
  ownership, body closure, malformed XML/scalars, gzip CRC and truncated trailer.
- Removing the decoder's final EOF validation makes the checksum, truncated
  trailer and trailing-document tests fail; restoring it makes them pass.
- Local ordinary unit tests, existing buffer suites, ordinary-test race checks
  for core/config/ext-v1, and `go vet ./...` passed. The isolated S3proxy/FUSE
  integration suite passed with 131 cases and 6 skips, using the newly built
  binary. This is not a claim that the full integration suite passed under the
  race detector; a pre-existing NoCloud test-harness race is tracked separately.

The follow-up changes were verified locally and are separate from the earlier
`25cf10b` production canary. Live RSS or CPU observations from that canary do not
measure this follow-up.
