"""Bounded, single-job document worker. No request accepts a filesystem path/URL."""

import hashlib
import hmac
import io
import json
import math
import multiprocessing
import os
import queue
import re
import signal
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import zipfile
from html.parser import HTMLParser
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from importlib.metadata import version
from pathlib import Path

from chunking import CHUNKER_VERSION, Chunker


def positive_env(name, default, maximum):
    value = int(os.environ.get(name, default))
    if not 0 < value <= maximum:
        raise ValueError(f"{name} is outside allowed range")
    return value


def settings():
    tokenizer_id = os.environ.get("TOKENIZER_ID", "Qwen/Qwen3-Embedding-0.6B")
    revision = os.environ.get("TOKENIZER_REVISION", "")
    if not re.fullmatch(r"[0-9a-f]{40}", revision):
        raise ValueError("TOKENIZER_REVISION must be a pinned 40-character commit SHA")
    embedding = os.environ.get("EMBEDDING_MODEL", tokenizer_id)
    if embedding != tokenizer_id:
        raise ValueError("EMBEDDING_MODEL and TOKENIZER_ID must identify the same model")
    return dict(tokenizer_id=tokenizer_id, tokenizer_revision=revision,
                embedding_model=embedding,
                token=os.environ.get("DOCUMENT_WORKER_TOKEN", ""),
                max_bytes=positive_env("MAX_FILE_BYTES", 32 * 1024 * 1024, 128 * 1024 * 1024),
                max_chars=positive_env("MAX_TEXT_CHARS", 1000000, 10000000),
                max_pages=positive_env("MAX_PAGES", 200, 1000),
                timeout=positive_env("PARSE_TIMEOUT_SECONDS", 180, 1800),
                tika_url=os.environ.get("TIKA_URL", ""))


def load_tokenizer(cfg):
    from tokenizers import Tokenizer
    from huggingface_hub import hf_hub_download

    # Only download tokenizer.json, never weights or remote executable code.
    path = hf_hub_download(repo_id=cfg["tokenizer_id"], filename="tokenizer.json",
                           revision=cfg["tokenizer_revision"],
                           local_files_only=os.environ.get("HF_HUB_OFFLINE") == "1")
    tokenizer = Tokenizer.from_file(path)
    tokenizer.no_truncation()
    tokenizer.no_padding()
    return tokenizer


def append_block(ir, text, kind, headings, page=None, spans=None, **extra):
    block_id = f"b{len(ir['blocks'])}"
    block = dict(block_id=block_id, type=kind, text=text, heading_path=list(headings),
                 reading_order=len(ir["blocks"]), **extra)
    supplied = []
    for source in spans or []:
        source = dict(source)
        start, end = source.get("start"), source.get("end")
        if not isinstance(start, int) or not isinstance(end, int) or start < 0 or end > len(text) or end < start:
            raise ValueError("invalid parser source span")
        if start < end:
            supplied.append(source)
    if not supplied:
        supplied = [dict(page_no=page, bbox=None, start=0, end=len(text))]
    covered, cursor, complete = sorted((s["start"], s["end"]) for s in supplied), 0, list(supplied)
    for start, end in covered:
        if cursor < start:
            complete.append(dict(page_no=None, bbox=None, start=cursor, end=start))
        cursor = max(cursor, end)
    if cursor < len(text):
        complete.append(dict(page_no=None, bbox=None, start=cursor, end=len(text)))
    block["source_spans"] = sorted(complete, key=lambda s: (s["start"], s["end"]))
    for span in block["source_spans"]:
        span["block_id"] = block_id
    ir["blocks"].append(block)
    return block


def push_heading(stack, level, text):
    """Retain explicit heading levels, including skipped levels and H2 starts."""
    while stack and stack[-1][0] >= level:
        stack.pop()
    stack.append((level, text))
    return [title for _, title in stack]


def append_table(ir, rows, headings, page=None, cells=None, table_spans=None, caption=""):
    """Rows are canonical source text; cells keep raw values and logical offsets."""
    table_id = f"t{len(ir['tables'])}"
    table = dict(table_id=table_id, caption=caption, heading_path=list(headings), cells=[], block_ids=[],
                 num_rows=len(rows), num_cols=max((len(r) for r in rows), default=0),
                 source_spans=table_spans or [], quality_flags=[])
    if cells:
        table["num_rows"] = max(table["num_rows"], max(c["row"] + c["row_span"] for c in cells))
        table["num_cols"] = max(table["num_cols"], max(c["column"] + c["col_span"] for c in cells))
    headers = ([cell["raw_text"] for cell in cells if cell.get("column_header")]
               if cells is not None else rows[0] if rows else [])
    column_paths = [[] for _ in range(table["num_cols"])]
    if cells is not None:
        for cell in cells:
            if cell.get("column_header"):
                for column in range(cell["column"], min(table["num_cols"], cell["column"] + cell["col_span"])):
                    column_paths[column].append(cell["raw_text"])
    else:
        column_paths = [[header] for header in headers]
    table["column_paths"] = column_paths
    for row_index, row in enumerate(rows):
        text, ranges, cursor = "", [], 0
        for column, value in enumerate(row):
            segment = (" | " if column else "") + value
            text += segment
            ranges.append((cursor, len(text)))
            cursor = len(text)
        # Empty rows have no searchable body, but remain represented in TableIR.
        if not text:
            continue
        row_spans = None
        if table_spans and len({s.get("page_no") for s in table_spans}) == 1:
            # A table-wide bbox locates a row only coarsely; it does not assert a
            # precise cell bbox or invent which page contains a multi-page row.
            row_spans = [dict(page_no=s.get("page_no"), bbox=s.get("bbox"), start=0, end=len(text))
                         for s in table_spans]
        block = append_block(ir, text, "table_row", headings, page, row_spans,
                             table_id=table_id, table_caption=caption, row_index=row_index, cell_ranges=ranges)
        table["block_ids"].append(block["block_id"])
        for column, value in enumerate(row):
            a, z = ranges[column]
            a += 3 if column else 0
            table["cells"].append(dict(cell_id=f"{table_id}r{row_index}c{column}",
                                       row=row_index, column=column, row_span=1, col_span=1,
                                       raw_text=value, column_header=row_index == 0,
                                       source_spans=[dict(block_id=block["block_id"], page_no=page,
                                                          bbox=None, start=a, end=z)]))
    if cells is not None:
        # Docling logical cells are authoritative for merges; row serialization
        # is only a retrieval representation, never the source for arithmetic.
        table["cells"] = cells
    ir["tables"].append(table)


def parse_text(data, markdown=False):
    text = data.decode("utf-8-sig", errors="strict").replace("\r\n", "\n").replace("\r", "\n")
    if "\x00" in text:
        raise ValueError("binary NUL is not valid document text")
    ir = dict(blocks=[], tables=[])
    headings, heading_stack, paragraph = [], [], []
    lines = text.splitlines()

    def flush():
        if paragraph:
            append_block(ir, "\n".join(paragraph), "paragraph", headings)
            paragraph.clear()

    i = 0
    while i < len(lines):
        line = lines[i]
        heading = re.match(r"^(#{1,6})\s+(.+?)\s*#*\s*$", line) if markdown else None
        if heading:
            flush()
            level, title = len(heading[1]), heading[2]
            headings = push_heading(heading_stack, level, title)
            append_block(ir, title, "heading", headings)
        elif markdown and re.match(r"^ {0,3}(`{3,}|~{3,})", line):
            flush()
            fence = re.match(r"^ {0,3}(`{3,}|~{3,})", line)[1]
            closing_fence = re.compile(rf"^ {{0,3}}{re.escape(fence[0])}{{{len(fence)},}}[ \t]*$")
            code = [line]
            i += 1
            while i < len(lines):
                code.append(lines[i])
                if closing_fence.fullmatch(lines[i]):
                    break
                i += 1
            append_block(ir, "\n".join(code), "code", headings)
        elif (markdown and i + 1 < len(lines) and "|" in line and
              re.fullmatch(r"\s*\|?\s*:?-{3,}:?\s*(\|\s*:?-{3,}:?\s*)+\|?\s*", lines[i + 1])):
            flush()
            rows = [split_table_row(line)]
            i += 2
            while i < len(lines) and "|" in lines[i] and lines[i].strip():
                rows.append(split_table_row(lines[i]))
                i += 1
            append_table(ir, rows, headings)
            continue
        elif re.match(r"^\s*(?:[-+*]|\d+[.)])\s+", line):
            flush()
            item = [line]
            while (i + 1 < len(lines) and lines[i + 1].startswith("  ") and
                   not re.match(r"^\s*(?:[-+*]|\d+[.)])\s+", lines[i + 1])):
                i += 1
                item.append(lines[i])
            append_block(ir, "\n".join(item), "list_item", headings)
        elif not line.strip():
            flush()
        else:
            paragraph.append(line)
        i += 1
    flush()
    return ir


def split_table_row(line):
    line = line.strip()
    if line.startswith("|"):
        line = line[1:]
    if re.search(r"(?<!\\)\|$", line):
        line = line[:-1]
    return [cell.strip().replace("\\|", "|") for cell in re.split(r"(?<!\\)\|", line)]


class XHTMLParser(HTMLParser):
    """Tika XHTML/HTML to IR; HTMLParser never fetches scripts, images or URLs."""

    def __init__(self):
        super().__init__(convert_charrefs=True)
        self.ir = dict(blocks=[], tables=[])
        self.headings, self.text, self.kind = [], [], "paragraph"
        self.heading_stack = []
        self.page, self.ignored, self.table, self.row, self.cell = None, 0, None, None, None
        self.caption, self.table_captions = None, []
        self.table_cells, self.occupied, self.row_index, self.col_index = [], set(), -1, 0
        self.cell_attrs = {}
        self.heading_level = 0

    def flush(self):
        value = "".join(self.text)
        if self.kind != "code":
            value = value.strip()
        self.text = []
        if not value.strip():
            return
        if self.heading_level:
            self.headings = push_heading(self.heading_stack, self.heading_level, value)
        append_block(self.ir, value, self.kind, self.headings, self.page)

    def handle_starttag(self, tag, attrs):
        attrs = dict(attrs)
        if tag in ("script", "style", "head"):
            self.ignored += 1
        if self.ignored:
            return
        if tag == "div" and "page" in attrs.get("class", "").split():
            self.flush()
            self.page = (self.page or 0) + 1
        if tag == "table":
            if self.table is not None:
                raise ValueError("nested tables require a richer parser")
            self.flush()
            self.table = []
            self.table_cells, self.occupied, self.row_index = [], set(), -1
            self.caption, self.table_captions = None, []
        elif self.table is not None:
            if tag == "caption":
                self.caption = []
            elif self.caption is not None:
                if tag == "br":
                    self.caption.append("\n")
            elif tag == "tr":
                self.row = []
                self.row_index += 1
                self.col_index = 0
            elif tag in ("td", "th"):
                row_span, col_span = int(attrs.get("rowspan", "1")), int(attrs.get("colspan", "1"))
                if not 0 < row_span <= 1000 or not 0 < col_span <= 1000:
                    raise ValueError("table span exceeds limit")
                while (self.row_index, self.col_index) in self.occupied:
                    self.col_index += 1
                row_header = tag == "th" and attrs.get("scope", "").strip().lower() in ("row", "rowgroup")
                self.cell_attrs = dict(row=self.row_index, column=self.col_index,
                                       row_span=row_span, col_span=col_span,
                                       column_header=tag == "th" and not row_header, row_header=row_header)
                if row_span * col_span > 10000:
                    raise ValueError("table span expansion exceeds limit")
                self.occupied.update((r, c) for r in range(self.row_index, self.row_index + row_span)
                                     for c in range(self.col_index, self.col_index + col_span))
                if len(self.occupied) > 200000:
                    raise ValueError("table cell count limit exceeded")
                self.cell = []
            elif tag == "br" and self.cell is not None:
                self.cell.append("\n")
        elif tag in ("p", "li", "pre", "h1", "h2", "h3", "h4", "h5", "h6"):
            self.flush()
            self.heading_level = int(tag[1]) if re.fullmatch(r"h[1-6]", tag) else 0
            self.kind = "heading" if self.heading_level else {"li": "list_item", "pre": "code"}.get(tag, "paragraph")
        elif tag == "br":
            self.text.append("\n")

    def handle_endtag(self, tag):
        if tag in ("script", "style", "head") and self.ignored:
            self.ignored -= 1
            return
        if self.ignored:
            return
        if self.table is not None:
            if tag == "caption" and self.caption is not None:
                self.table_captions.append("".join(self.caption).strip())
                self.caption = None
            elif self.caption is not None:
                return
            elif tag in ("td", "th") and self.cell is not None:
                if self.row is None:
                    self.row = []
                while len(self.row) <= self.col_index:
                    self.row.append("")
                value = "".join(self.cell).strip()
                self.row[self.col_index] = value
                self.table_cells.append(dict(**self.cell_attrs, raw_text=value,
                                              cell_id=f"t{len(self.ir['tables'])}cell{len(self.table_cells)}",
                                              source_spans=[]))
                self.col_index += self.cell_attrs["col_span"]
                self.cell = None
            elif tag == "tr" and self.row is not None:
                self.table.append(self.row)
                self.row = None
            elif tag == "table":
                append_table(self.ir, self.table, self.headings, self.page, cells=self.table_cells,
                             caption="\n".join(self.table_captions))
                attach_cell_spans(self.ir, self.ir["tables"][-1])
                self.table = None
                self.caption, self.table_captions = None, []
        elif tag in ("p", "li", "pre", "h1", "h2", "h3", "h4", "h5", "h6", "div"):
            self.flush()
            self.heading_level, self.kind = 0, "paragraph"

    def handle_data(self, data):
        if self.ignored:
            return
        if self.caption is not None:
            self.caption.append(data)
        elif self.cell is not None:
            self.cell.append(data)
        elif self.table is None:
            self.text.append(data)


def parse_html(data):
    parser = XHTMLParser()
    parser.feed(data.decode("utf-8-sig", errors="strict"))
    parser.close()
    parser.flush()
    if parser.table is not None:
        raise ValueError("unterminated table")
    return parser.ir


def normalize_bbox(bbox, page):
    if bbox is None or page is None:
        return None
    width, height = page.size.width, page.size.height
    if not width or not height:
        return None
    origin = getattr(bbox.coord_origin, "value", str(bbox.coord_origin))
    top, bottom = bbox.t, bbox.b
    if origin == "BOTTOMLEFT":
        top, bottom = height - top, height - bottom
    # Normalized display coordinates, top-left origin. Missing positions stay null.
    return [max(0.0, min(1.0, n)) for n in (bbox.l / width, min(top, bottom) / height,
                                            bbox.r / width, max(top, bottom) / height)]


def docling_ir(document):
    ir, headings, heading_stack = dict(blocks=[], tables=[]), [], []
    for item, _ in document.iterate_items():
        label = getattr(item.label, "value", str(item.label))
        text = getattr(item, "text", "")
        origins = []
        for prov in getattr(item, "prov", []):
            a, z = getattr(prov, "charspan", (0, len(text)))
            origins.append(dict(page_no=prov.page_no,
                                bbox=normalize_bbox(prov.bbox, document.pages.get(prov.page_no)),
                                start=max(0, a), end=min(len(text), z)))
        if label == "table":
            rows = []
            # Do not use grid: it replicates merged cells, corrupting aggregates.
            cells = []
            table_id = f"t{len(ir['tables'])}"
            for index, cell in enumerate(item.data.table_cells):
                r, c = cell.start_row_offset_idx, cell.start_col_offset_idx
                while len(rows) <= r:
                    rows.append([])
                while len(rows[r]) <= c:
                    rows[r].append("")
                rows[r][c] = cell.text
                cells.append(dict(cell_id=f"{table_id}cell{index}", row=r, column=c,
                                  row_span=cell.row_span, col_span=cell.col_span,
                                  raw_text=cell.text, column_header=cell.column_header,
                                  row_header=cell.row_header, source_spans=[]))
            caption = "\n".join(ref.resolve(document).text for ref in getattr(item, "captions", []))
            append_table(ir, rows, headings, cells=cells, table_spans=origins, caption=caption)
            # Resolve cell offsets into canonical row blocks without claiming
            # Docling supplied fine-grained per-cell page provenance.
            attach_cell_spans(ir, ir["tables"][-1])
            continue
        if not text:
            continue
        if label in ("title", "section_header"):
            level = 1 if label == "title" else getattr(item, "level", 1)
            headings = push_heading(heading_stack, level, text)
            kind = "heading"
        else:
            kind = {"list_item": "list_item", "code": "code", "formula": "formula",
                    "caption": "caption", "footnote": "footnote"}.get(label, "paragraph")
        append_block(ir, text, kind, headings, spans=origins)
    return ir


def attach_cell_spans(ir, table):
    by_row = {b["row_index"]: b for b in ir["blocks"] if b.get("table_id") == table["table_id"]}
    for cell in table["cells"]:
        block = by_row.get(cell["row"])
        if block and cell["column"] < len(block["cell_ranges"]):
            a, z = block["cell_ranges"][cell["column"]]
            a += 3 if cell["column"] else 0
            cell["source_spans"] = [dict(block_id=block["block_id"], page_no=s["page_no"],
                                         bbox=s["bbox"], start=a, end=z) for s in block["source_spans"]]


class Parser:
    def __init__(self, cfg):
        self.cfg = cfg
        self.tokenizer = load_tokenizer(cfg)
        self.converters = {}

    def pdf(self, data):
        from pypdf import PdfReader
        from docling.datamodel.base_models import DocumentStream, InputFormat, ConversionStatus
        from docling.datamodel.pipeline_options import PdfPipelineOptions, TesseractCliOcrOptions
        from docling.document_converter import DocumentConverter, PdfFormatOption

        reader = PdfReader(io.BytesIO(data))
        if reader.is_encrypted:
            raise ValueError("encrypted PDF is not supported")
        if len(reader.pages) > self.cfg["max_pages"]:
            raise ValueError("PDF exceeds page limit")
        # ponytail: conservative text/raster routing, add area/reading-order
        # quality metrics when the evaluation set requires precise page routing.
        needs_ocr = any(page_needs_ocr(page) for page in reader.pages)
        if needs_ocr not in self.converters:
            options = PdfPipelineOptions(do_ocr=needs_ocr, do_table_structure=True,
                                         document_timeout=self.cfg["timeout"],
                                         enable_remote_services=False, allow_external_plugins=False)
            options.ocr_options = TesseractCliOcrOptions(lang=["chi_sim", "eng"], force_full_page_ocr=False)
            artifacts = os.environ.get("DOCLING_ARTIFACTS_PATH")
            if artifacts:
                options.artifacts_path = artifacts
            self.converters[needs_ocr] = DocumentConverter(allowed_formats=[InputFormat.PDF],
                format_options={InputFormat.PDF: PdfFormatOption(pipeline_options=options)})
        # In-memory stream avoids both URL access and leftover uploaded files
        # when the watchdog terminates a parser that exceeds its deadline.
        source = DocumentStream(name="input.pdf", stream=io.BytesIO(data))
        result = self.converters[needs_ocr].convert(source, max_num_pages=self.cfg["max_pages"],
                                                  max_file_size=self.cfg["max_bytes"])
        if result.status != ConversionStatus.SUCCESS:
            raise ValueError("PDF conversion was partial or failed; original retained for retry")
        return docling_ir(result.document), "docling-ocr" if needs_ocr else "docling-native", version("docling")

    def parse(self, data, filename, content_type):
        if not data or len(data) > self.cfg["max_bytes"]:
            raise ValueError("empty file or size limit exceeded")
        suffix = Path(filename).suffix.lower()
        if data.startswith(b"%PDF-") or suffix == ".pdf":
            if not data.startswith(b"%PDF-"):
                raise ValueError("PDF signature mismatch")
            ir, parser, parser_version = self.pdf(data)
        elif suffix in (".txt", ".md", ".markdown"):
            ir, parser, parser_version = parse_text(data, suffix != ".txt"), "structured-text", "1"
        elif suffix in (".html", ".htm", ".xhtml"):
            ir, parser, parser_version = parse_html(data), "html-stdlib", "1"
        else:
            excel_mime = {".xls": "application/vnd.ms-excel",
                          ".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"}
            allowed = {".doc", ".docx", ".ppt", ".pptx", ".odt", ".rtf", ".eml", ".msg", *excel_mime}
            if suffix not in allowed or not self.cfg["tika_url"]:
                raise ValueError("unsupported file type or TIKA_URL is not configured")
            if data.startswith(b"PK"):
                validate_archive(data, self.cfg["max_bytes"] * 8)
            content_type = excel_mime.get(suffix, content_type)
            ir, parser, parser_version = parse_html(self.tika(data, content_type)), "tika-xhtml", "server"
        if sum(len(b["text"]) for b in ir["blocks"]) > self.cfg["max_chars"]:
            raise ValueError("extracted text exceeds character limit")
        parents, children = Chunker(self.tokenizer, filename).chunks(ir)
        if not children:
            raise ValueError("no searchable content extracted")
        ir["file_name"] = filename
        ir["sha256"] = hashlib.sha256(data).hexdigest()
        return dict(schema_version="document-v1", parser=parser, parser_version=parser_version,
                    tokenizer_id=self.cfg["tokenizer_id"], tokenizer_revision=self.cfg["tokenizer_revision"],
                    embedding_model=self.cfg["embedding_model"], chunker_version=CHUNKER_VERSION,
                    ir=ir, parents=parents, children=children)

    def tika(self, data, content_type):
        class NoRedirect(urllib.request.HTTPRedirectHandler):
            def redirect_request(self, *args, **kwargs):
                raise ValueError("Tika redirects are not allowed")

        request = urllib.request.Request(self.cfg["tika_url"].rstrip("/") + "/tika", data=data, method="PUT",
            headers={"Accept": "application/xhtml+xml", "Content-Type": content_type,
                     "X-Tika-Skip-Embedded": "true"})
        with urllib.request.build_opener(NoRedirect).open(request, timeout=self.cfg["timeout"]) as response:
            body = response.read(self.cfg["max_chars"] * 8 + 1)
        if len(body) > self.cfg["max_chars"] * 8:
            raise ValueError("Tika output exceeds limit")
        return body


def page_needs_ocr(page):
    """Inspect only rendered images and use their effective page-space size."""
    from pypdf.generic import ContentStream

    # ponytail: interpreting q/Q, cm and Do is enough for scan routing; a full
    # PDF renderer belongs in Docling. Uncertain streams conservatively use OCR.
    active, inspected = set(), 0

    def multiply(left, right):
        return [left[0] * right[0] + left[1] * right[2],
                left[0] * right[1] + left[1] * right[3],
                left[2] * right[0] + left[3] * right[2],
                left[2] * right[1] + left[3] * right[3],
                left[4] * right[0] + left[5] * right[2] + right[4],
                left[4] * right[1] + left[5] * right[3] + right[5]]

    def rendered_large(matrix):
        return min(math.hypot(matrix[0], matrix[1]), math.hypot(matrix[2], matrix[3])) >= 200

    def operations(stream):
        if hasattr(stream, "operations"):
            return stream.operations
        return ContentStream(stream, getattr(page, "pdf", None)).operations

    def scan(stream, resources, matrix=None, depth=0):
        nonlocal inspected
        if depth > 16:
            return True
        matrix = matrix or [1, 0, 0, 1, 0, 0]
        resources = resources.get_object() if hasattr(resources, "get_object") else resources
        objects = resources.get("/XObject", {})
        objects = objects.get_object() if hasattr(objects, "get_object") else objects
        stack = []
        for operands, operator in operations(stream):
            if operator == b"q":
                stack.append(list(matrix))
                continue
            if operator == b"Q":
                if not stack:
                    raise ValueError("unbalanced PDF graphics state")
                matrix = stack.pop()
                continue
            if operator == b"cm":
                if len(operands) < 6:
                    raise ValueError("invalid PDF transformation")
                matrix = multiply([float(value) for value in operands[:6]], matrix)
                continue
            if operator == b"INLINE IMAGE":
                inspected += 1
                if inspected > 1024:
                    return True
                if rendered_large(matrix):
                    return True
                continue
            if operator != b"Do" or not operands:
                continue
            inspected += 1
            if inspected > 1024:
                return True
            reference = objects.get(operands[0])
            if reference is None:
                raise ValueError("drawn PDF object is missing")
            obj = reference.get_object() if hasattr(reference, "get_object") else reference
            identity = id(obj)
            if identity in active:
                return True
            if obj.get("/Subtype") == "/Image" and rendered_large(matrix):
                return True
            if obj.get("/Subtype") == "/Form":
                active.add(identity)
                form_matrix = obj.get("/Matrix", [1, 0, 0, 1, 0, 0])
                form_matrix = form_matrix.get_object() if hasattr(form_matrix, "get_object") else form_matrix
                form_matrix = [float(value) for value in form_matrix]
                if len(form_matrix) != 6:
                    raise ValueError("invalid PDF Form transformation")
                if scan(obj, obj.get("/Resources", resources), multiply(form_matrix, matrix), depth + 1):
                    return True
                active.remove(identity)
        return False

    try:
        content = page.get_contents()
        if content is not None and scan(content, page.get("/Resources", {})):
            return True
        return len((page.extract_text() or "").strip()) < 30
    except Exception:
        # Broken references/text streams must not be mistaken for native-only.
        return True


def validate_archive(data, max_expanded):
    with zipfile.ZipFile(io.BytesIO(data)) as archive:
        entries = archive.infolist()
        if len(entries) > 10000 or sum(e.file_size for e in entries) > max_expanded:
            raise ValueError("archive expansion limit exceeded")
        for entry in entries:
            if entry.flag_bits & 1 or entry.file_size > max_expanded:
                raise ValueError("encrypted or oversized archive entry")


def process_loop(connection, cfg):
    try:
        os.environ["TMPDIR"] = cfg["temp_dir"]
        tempfile.tempdir = cfg["temp_dir"]
        if os.name != "nt":
            import resource
            os.setsid()
            resource.setrlimit(resource.RLIMIT_CORE, (0, 0))
            resource.setrlimit(resource.RLIMIT_NOFILE, (512, 512))
        parser = Parser(cfg)
        connection.send((True, "ready"))
    except Exception as exc:
        connection.send((False, f"initialization failed: {type(exc).__name__}: {exc}"))
        return
    while True:
        try:
            data, filename, content_type = connection.recv()
        except EOFError:
            return
        try:
            result = parser.parse(data, filename, content_type)
            payload = json.dumps(result, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
            if len(payload) > cfg["max_chars"] * 32:
                raise ValueError("serialized output exceeds limit")
            connection.send((True, payload))
        except Exception as exc:
            # Input text/file contents are never copied into HTTP errors or logs.
            connection.send((False, f"{type(exc).__name__}: document conversion failed"))


class ProcessRunner:
    """One resident parser process; hard deadlines kill the entire parser job."""

    def __init__(self, cfg):
        self.cfg, self.process, self.connection = cfg, None, None
        self.temp_dir = None

    def close(self):
        if self.connection:
            self.connection.close()
        if self.process:
            if os.name != "nt" and self.process.is_alive():
                try:
                    os.killpg(self.process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    self.process.terminate()
            elif self.process.is_alive():
                self.process.terminate()
            self.process.join(5)
            if self.process.is_alive():
                self.process.kill()
                self.process.join(5)
        self.process, self.connection = None, None
        if self.temp_dir:
            self.temp_dir.cleanup()
            self.temp_dir = None

    def receive(self, timeout=None):
        timeout = self.cfg["timeout"] if timeout is None else max(0, timeout)
        result, connection = queue.Queue(maxsize=1), self.connection

        def read_result():
            try:
                result.put((None, connection.recv()))
            except Exception as exc:
                result.put((exc, None))

        threading.Thread(target=read_result, daemon=True).start()
        try:
            error, message = result.get(timeout=timeout)
        except queue.Empty:
            self.close()
            raise TimeoutError("document worker deadline exceeded")
        if error is not None:
            self.close()
            raise RuntimeError("parser process exited") from error
        success, value = message
        if not success:
            raise ValueError(value)
        return value

    def run(self, data, filename, content_type):
        deadline = time.monotonic() + self.cfg["timeout"]
        if not self.process or not self.process.is_alive():
            self.close()
            context = multiprocessing.get_context("spawn")
            self.temp_dir = tempfile.TemporaryDirectory(prefix="paismart-document-")
            self.connection, child = context.Pipe()
            self.process = context.Process(target=process_loop, args=(child, {**self.cfg, "temp_dir": self.temp_dir.name}))
            self.process.start()
            child.close()
            try:
                self.receive(deadline - time.monotonic())
            except Exception:
                self.close()
                raise
        self.connection.send((data, filename, content_type))
        return self.receive(deadline - time.monotonic())


class WorkerHTTPServer(ThreadingHTTPServer):
    daemon_threads = True
    request_queue_size = 4

    def __init__(self, *args, **kwargs):
        # One parser plus a few short health/busy handlers; never grow an
        # unbounded thread set merely to keep liveness independent of parsing.
        self.request_slots = threading.BoundedSemaphore(4)
        self.parse_lock = threading.Lock()
        super().__init__(*args, **kwargs)

    def process_request(self, request, client_address):
        if not self.request_slots.acquire(blocking=False):
            self.shutdown_request(request)
            return
        try:
            super().process_request(request, client_address)
        except Exception:
            self.request_slots.release()
            raise

    def process_request_thread(self, request, client_address):
        try:
            super().process_request_thread(request, client_address)
        finally:
            self.request_slots.release()


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.0"

    def setup(self):
        super().setup()
        self.connection.settimeout(15)

    def log_message(self, fmt, *args):
        pass  # Do not log uploaded names, Authorization headers, or parser input.

    def respond(self, code, value):
        body = value if isinstance(value, bytes) else json.dumps(value).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        self.respond(200 if self.path == "/health" else 404,
                     {"status": "alive", "schema_version": "document-v1"})

    def do_POST(self):
        cfg = self.server.cfg
        if self.path != "/parse":
            self.respond(404, {"error": "not found"})
            return
        if cfg["token"] and not hmac.compare_digest(self.headers.get("Authorization", ""), "Bearer " + cfg["token"]):
            self.respond(401, {"error": "unauthorized"})
            return
        if not self.server.parse_lock.acquire(blocking=False):
            self.respond(503, {"error": "document parser is busy"})
            return
        try:
            if self.headers.get("Transfer-Encoding") or len(self.headers.get_all("Content-Length", [])) != 1:
                raise ValueError("exactly one Content-Length is required; chunked input is unsupported")
            length = int(self.headers["Content-Length"])
            if not 0 < length <= cfg["max_bytes"]:
                self.respond(413, {"error": "file size limit exceeded"})
                return
            filename = urllib.parse.unquote(self.headers.get("X-File-Name", ""), errors="strict")
            if (not filename or len(filename) > 1024 or any(c in filename for c in "\x00\r\n/\\") or
                    filename in (".", "..") or ":" in filename):
                raise ValueError("X-File-Name must be an encoded basename, not a path or URL")
            content_type = self.headers.get("Content-Type", "application/octet-stream")
            deadline, parts, remaining = time.monotonic() + 30, [], length
            while remaining:
                budget = deadline - time.monotonic()
                if budget <= 0:
                    raise TimeoutError("upload read deadline exceeded")
                self.connection.settimeout(min(15, budget))
                part = self.rfile.read1(min(65536, remaining))
                if not part:
                    break
                parts.append(part)
                remaining -= len(part)
            data = b"".join(parts)
            if len(data) != length:
                raise ValueError("incomplete body")
            result = self.server.runner.run(data, filename, content_type)
            self.respond(200, result)
        except TimeoutError:
            self.respond(504, {"error": "document processing timed out; retry is safe"})
        except (ValueError, UnicodeError):
            self.respond(422, {"error": "invalid document, configuration, or processing limit exceeded"})
        except (OSError, RuntimeError):
            self.respond(503, {"error": "document parser unavailable"})
        finally:
            self.server.parse_lock.release()


def main():
    cfg = settings()
    address = os.environ.get("WORKER_HOST", "127.0.0.1")
    if address not in ("127.0.0.1", "localhost", "::1") and not cfg["token"]:
        raise ValueError("DOCUMENT_WORKER_TOKEN is required when binding a non-loopback address")
    # ponytail: one active job per worker provides bounded concurrency. Scale
    # worker replicas if measured queueing requires additional parsing capacity.
    server = WorkerHTTPServer((address, int(os.environ.get("WORKER_PORT", "8091"))), Handler)
    server.cfg, server.runner = cfg, ProcessRunner(cfg)
    try:
        server.serve_forever()
    finally:
        server.runner.close()
        server.server_close()


if __name__ == "__main__":
    main()
