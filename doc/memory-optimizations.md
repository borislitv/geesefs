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
of reduced steady-state RSS on a read-only workload. A data cache can legitimately
retain file data up to its configured budget. The changes do not compress or
reinterpret stored package/archive bytes.

## Optional kernel cache bypass

`KeepPageCache=false` invalidates old kernel data when opening a file; it does
not stop subsequent reads from populating the kernel page cache. Consequently,
ordinary reads can retain the same payload in both GeeseFS and the kernel.

`--read-only-direct-io -o ro` sets FUSE direct I/O on read-only file handles to
avoid this second copy for ordinary reads. It is opt-in, leaves the GeeseFS
cache budget and readahead unchanged, and requires an unambiguous read-only
mount. Omitting `ro` or combining it with `rw` returns an error before backend
initialization. Existing writable/create-handle behavior is unchanged.

With the current FUSE dependency on Linux, shared mappings fail with `ENODEV`;
private mappings remain supported and can populate the kernel page cache.
Use this option only when clients do not require shared `mmap`. More FUSE
requests may reach GeeseFS because the kernel no longer satisfies ordinary
reads from its data cache. Compatibility and performance depend on the client,
kernel and workload; the option is not enabled automatically.

### Isolated payload measurement

On 2026-10-26, Linux amd64, Go 1.25.14, five interleaved trials per mode used the
same binary and local S3 emulator. Each trial mounted a fresh filesystem in a
fresh cgroup v2 with a 1-GiB limit, `GOMAXPROCS=4`, `GOMEMLIMIT=512MiB`, a
128-MiB GeeseFS data cache and a one-hour stat TTL. Eight 8-MiB objects were
downloaded through a loopback rsync daemon, once cold and once warm.

The destination was tmpfs, removed before every memory sample. Every downloaded
object was checked against its full SHA-256. Each trial also checked an nginx
server with `sendfile on`, a complete 8-MiB response and two byte ranges. All
byte comparisons and ordinary unmounts passed in both modes. No GC was requested
for the samples.

A separate compatibility run in each mode used `rsync --checksum --no-whole-file`
to repair locally corrupted copies. Both reused matched data and produced the
exact original hashes, rather than silently exercising whole-file transfers.

| Median after warm rsync, destination removed | Default | Direct I/O |
| --- | ---: | ---: |
| Whole test cgroup, `memory.current` | 153.07 MiB | 89.25 MiB |
| `memory.stat`: `file - shmem` | 64.015625 MiB | 0.015625 MiB |
| GeeseFS `HeapAlloc`, natural heap sample | 71.41 MiB | 71.32 MiB |

The ordinary-read workload retained exactly 64 MiB less non-shmem file cache,
matching the fixture payload. The Go heap was essentially unchanged: this is
kernel-cache duplication removed, not a smaller GeeseFS cache or a demonstrated
reduction in process RSS. Whole-cgroup savings are specific to this fixture.

Cold rsync medians in this batch were 153.2 versus 189.1 MiB/s, warm medians
239.2 versus 272.9 MiB/s. Short-run throughput varied between batches; these
numbers do not establish a general speedup or production throughput result.

To reproduce the memory comparison, use a dedicated test bucket with eight
8-MiB objects and the same GeeseFS binary and budgets for both modes. Place the
reader, rsync daemon and receiving client in a fresh accounting-enabled cgroup
for each trial; keep the S3 emulator outside it. Record `memory.current` and
`memory.stat` before reads and after removing each tmpfs destination. Compare
`file - shmem`, not `file` alone, and retain full-file hashes. Interleave trials
with and without `--read-only-direct-io`; do not drop global caches or force GC.

`TestReadOnlyDirectIOOpen` checks access modes and default behavior.
`TestReadOnlyDirectIORequiresReadOnlyMount` checks rejected configurations without
backend or mount side effects. The mounted tests `TestReadOnlyDirectIOFuse` and
`TestReadOnlyKernelCacheFuse` exercise real full, unaligned, concurrent and EOF
reads, empty files, reopening, private/shared mappings and rejected writes.
Removing the direct-I/O response flag makes both the open-contract test and
the real shared-mapping test fail.

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

The benchmark measures allocation and decoding work in an isolated fixture.
Production RSS and throughput improvements require separate measurements under
comparable workloads.
