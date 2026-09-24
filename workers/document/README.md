# Document Worker (first version)

The document worker is a required service in the project's only document
processing pipeline. All uploaded documents use structured parsing and
parent/child chunking. Configure the Go embedding service and worker with
**the same model and tokenizer revision** before starting document processing.

## Run

From the repository root, set the following shell environment variables (or
put them in the project's local, uncommitted `.env` file):

| Compose environment | Worker environment | Requirement |
| --- | --- | --- |
| `DOCUMENT_WORKER_TOKEN` | `DOCUMENT_WORKER_TOKEN` | Required nonempty secret, shared with the Go client |
| `DOCUMENT_TOKENIZER_REVISION` | `TOKENIZER_REVISION` | Required exact 40-character model commit, never `main` |
| `DOCUMENT_TOKENIZER_ID` | `TOKENIZER_ID` | Defaults to `Qwen/Qwen3-Embedding-0.6B` |
| `DOCUMENT_EMBEDDING_MODEL` | `EMBEDDING_MODEL` | Defaults to `Qwen/Qwen3-Embedding-0.6B`; must match tokenizer and Go embedding model |

```sh
docker compose -f deployments/docker-compose.yaml up -d --build
```

The main Compose file builds the worker and starts it alongside the existing
infrastructure. It supplies `TIKA_URL=http://tika:9998`, persists model files in
the `document-models` named volume, and binds the worker only to host loopback
at `http://127.0.0.1:8091`. No extra Compose file is needed. Keep the worker URL,
shared token, model identity and fixed tokenizer revision consistent with the
Go configuration. If Go runs inside the Compose network, use
`http://document-worker:8091` instead of the host-loopback URL.

The tokenizer is loaded once with `hf_hub_download(..., revision=...)` and
`Tokenizer.from_file`; only `tokenizer.json` is downloaded for token counting.
No model-provided Python code is executed. The worker disables tokenizer
truncation and padding. Counted input is the final `embedding_text`, including
metadata and the tokenizer's configured special-token processor; do not add a
different provider-side prompt without updating this contract.
The Go `document_processing.max_file_bytes` limit must match `MAX_FILE_BYTES`;
the upload UI reads the Go limit before hashing or sending a file.

Models may need downloading on the first request. Prepare a persistent model
cache before using `HF_HUB_OFFLINE=1` or restricting outbound network traffic.
For a predownloaded/offline deployment, mount Docling artifacts and explicitly
configure `DOCLING_ARTIFACTS_PATH` and `HF_HUB_OFFLINE` in the service environment.
Model downloads are not included in the unit tests or automatically performed
by those tests. Container memory/PID/CPU limits are part of the deployment
boundary, not optional protection provided by the Python heap.

| Environment | Default |
| --- | --- |
| `TOKENIZER_ID` / `EMBEDDING_MODEL` | `Qwen/Qwen3-Embedding-0.6B`; must match |
| `TOKENIZER_REVISION` | Required fixed commit, no `main` |
| `DOCUMENT_WORKER_TOKEN` | Required for non-loopback bind |
| `WORKER_HOST` / `WORKER_PORT` | `127.0.0.1` / `8091`; container binds `0.0.0.0` |
| `MAX_FILE_BYTES` | 33554432 |
| `MAX_TEXT_CHARS` | 1000000 |
| `MAX_PAGES` | 200 |
| `PARSE_TIMEOUT_SECONDS` | 180 total, including any model initialization and parse |
| `TIKA_URL` | Main Compose sets `http://tika:9998`; office/email parsing requires it |

## HTTP contract

`POST /parse` takes raw file bytes, an exact `Content-Length`, the original
`Content-Type`, percent-encoded basename in `X-File-Name`, and
`Authorization: Bearer <secret>`. Paths, URLs, empty files, chunked bodies,
oversized requests, and unsupported formats are rejected. `GET /health` reports
process liveness only, not model readiness.

The JSON response follows `internal/model/document_ir.go`: schema
`document-v1`, parser/tokenizer/model/chunker identities, complete `ir`, and
`parents`/`children`. `p0`/`c0` are document-local IDs; Go assigns document,
owner, permission and processing-version identities. Spans are half-open
Unicode character offsets in the identified IR block. Pages are one-based and
bounding boxes normalized to `[left, top, right, bottom]` with top-left origin.
Unknown page/geometry stays null. Both parent and child chunks persist
`context_prefix` for answer generation. Child `embedding_text` equals
`context_prefix + body_text`; the body remains source text. Parent `token_count`
includes the prefix, although its `embedding_text` is empty because parents
are not embedded.

Structural boundaries have **zero overlap**. A complete structure of at most
600 final-input tokens is never split just to reach the 450 target. Only an
oversized text unit receives sentence-boundary fallback (code uses complete
lines), without overlap. Only a single oversized sentence or code line receives
token fallback; continuations overlap at most 75 tokens within that unit and parent.
Parents target 1500, at most 2000 tokens, without overlap. Headers stay in IR
and `heading_path`, not standalone empty-content vectors. Table rows use
separate structural packing; oversized rows split at cells, oversized cells
retain text continuations without overlap. Logical cell IDs/merges/raw values
remain in TableIR. This version does not produce synthetic table summaries or
execute numeric calculations.
Table captions, column units and row labels are retained in the context prefix;
code continuations retain their language. Necessary table evidence is never
truncated: an indivisible prefix that leaves no room for source text fails
explicitly. Row labels use parser-marked row headers, otherwise the first
nonempty data cell; they are not inferred semantic primary keys.

## Supported parsing and current limits

- UTF-8 TXT / Markdown: standard-library paragraph/list/ATX-heading/fenced-code
  and simple pipe-table parsing, preserving empty first/last cells. Other Markdown syntax stays literal text;
  it is not claimed to be a full CommonMark AST.
- HTML and Tika XHTML: no scripts, stylesheet execution or external resource
  fetches. Headings, paragraphs, lists, page divs, and merged table cells are
  retained. Preformatted code keeps indentation and trailing newlines. Nested tables fail explicitly.
- PDF: Docling 2.55.1, with OCR off when every page has sufficient embedded
  text and no substantial raster image; otherwise Docling's region-based
  Tesseract Chinese/English OCR is enabled (including scans with a native
  header/watermark). Raster checks follow only actually rendered `Do`/inline images,
  compose their CTMs through nested Forms, and use effective page-space size. Depth/object
  limits, cycles, damaged streams or exhausted limits conservatively enable OCR.
  This conservative whole-document switch is not the architecture's
  complete per-page quality router. Partial conversions fail rather than
  publishing partial documents. PDF positions are retained. Multi-page table
  rows with no reliable per-row page mapping remain unlocated; table-level
  page provenance is retained separately.
- DOC/DOCX/XLS/XLSX/PPT/PPTX/ODT/RTF/EML/MSG: forwarded only to the administrator's
  configured Tika server, with embedded extraction disabled. Tika must be
  isolated and resource-limited separately; a worker timeout cannot kill a
  remote Tika task. Excel MIME types are supplied explicitly, including when
  the caller sends `application/octet-stream`. Archive expansion limits are
  checked before forwarding ZIP containers such as XLSX; no separate Excel
  parsing engine is used.

The service runs one resident parser process at a time; concurrent parse requests
are rejected as busy while a small bounded thread pool keeps health responsive. Timeout covers
the complete result receive and kills the parser's
process group on Linux (including OCR subprocesses), clears its private temp
directory, and permits a fresh parser on retry. The Windows development path
terminates the Python process, but is not a replacement for Linux container
process/resource isolation. The service is intended for a private backend
network, not direct public exposure.

Deferred: PaddleOCR-VL/MinerU/Camelot candidate routing, cross-page table merging,
formula enrichment, normalized numeric cells/SQL calculations, synthetic
table-summary indexing, and pixel-level page quality evaluation.

## Checks

```sh
python -m unittest discover -s workers/document -p 'test_*.py' -v
```

The tests use an explicitly fake character tokenizer to exercise exact
boundaries, complete coverage, provenance, structural non-overlap, bounded
fallback overlap, table cases, authentication, paths, size limits, and timeout
cleanup. If `tokenizers` is installed, an additional locally trained ByteLevel
BPE test exercises real Unicode offsets and special tokens (otherwise it
skips). These tests do not claim Docling/OCR quality or a real Qwen integration
test.

Verified API references:
[Docling pinned pipeline options](https://github.com/docling-project/docling/blob/v2.55.1/docling/datamodel/pipeline_options.py),
[Docling pinned dependency contract](https://github.com/docling-project/docling/blob/v2.55.1/pyproject.toml),
[Docling document/cell schema](https://github.com/docling-project/docling-core/blob/v2.48.4/docling_core/types/doc/document.py),
[Tokenizers pipeline](https://huggingface.co/docs/tokenizers/python/master/pipeline.html).
