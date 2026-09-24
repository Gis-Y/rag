import http.client
import io
import json
import os
import threading
import time
import unittest
import urllib.parse
import zipfile
from types import SimpleNamespace
from unittest.mock import patch

from chunking import Chunker
from worker import Handler, Parser, ProcessRunner, WorkerHTTPServer, docling_ir, page_needs_ocr, parse_html, parse_text, settings, validate_archive


class CharacterTokenizer:
    """Deterministic test double, deliberately NOT used by the production worker."""

    def encode(self, text, add_special_tokens=True):
        return SimpleNamespace(ids=list(range(len(text) + (2 if add_special_tokens else 0))),
                               offsets=[(i, i + 1) for i in range(len(text))])


class ChunkTests(unittest.TestCase):
    def setUp(self):
        self.chunker = Chunker(CharacterTokenizer())

    def verify(self, ir, parents, children):
        by_parent = {p["chunk_id"]: p for p in parents}
        by_block = {b["block_id"]: b for b in ir["blocks"]}
        coverage = {b["block_id"]: set() for b in ir["blocks"] if b["type"] != "heading" and b["text"]}
        parent_coverage = {block_id: set() for block_id in coverage}
        for parent in parents:
            self.assertLessEqual(parent["token_count"], 2000)
            for s in parent["source_spans"]:
                parent_coverage[s["block_id"]].update(range(s["start"], s["end"]))
        for child in children:
            self.assertLessEqual(child["token_count"], 600)
            parent = by_parent[child["parent_id"]]
            self.assertEqual(child["token_count"], self.chunker.count(child["embedding_text"]))
            for s in child["source_spans"]:
                self.assertTrue(any(p["block_id"] == s["block_id"] and p["start"] <= s["start"] and p["end"] >= s["end"]
                                    for p in parent["source_spans"]))
                coverage[s["block_id"]].update(range(s["start"], s["end"]))
            for s in child["overlap_spans"]:
                self.assertLessEqual(self.chunker.count(by_block[s["block_id"]]["text"][s["start"]:s["end"]]), 75)
        for block in ir["blocks"]:
            if block["block_id"] in coverage:
                expected = set(range(len(block["text"])))
                self.assertEqual(coverage[block["block_id"]], expected)
                self.assertEqual(parent_coverage[block["block_id"]], expected)

    def test_structure_has_no_overlap(self):
        ir = parse_text(("甲" * 350 + "\n\n" + "乙" * 350).encode())
        parents, children = self.chunker.chunks(ir)
        self.assertEqual(len(children), 2)
        self.assertTrue(all(not c["overlap_spans"] for c in children))
        self.verify(ir, parents, children)

    def test_exact_600_and_target_do_not_split(self):
        for length in (450, 598):
            ir = parse_text(("a" * length).encode())
            parents, children = self.chunker.chunks(ir)
            self.assertEqual(len(children), 1)
            self.assertEqual(children[0]["body_text"], "a" * length)
            self.verify(ir, parents, children)

    def test_long_atom_coverage_across_parents(self):
        ir = parse_text(("这是没有句号的一段🧪" * 650).encode())
        parents, children = self.chunker.chunks(ir)
        self.assertGreater(len(parents), 1)
        self.assertTrue(any(c["overlap_spans"] for c in children))
        for parent in parents:
            self.assertFalse(next(c for c in children if c["parent_id"] == parent["chunk_id"])["overlap_spans"])
        self.verify(ir, parents, children)

    def test_overlap_stops_at_paragraph(self):
        ir = parse_text(("甲" * 900 + "\n\n" + "乙" * 100).encode())
        parents, children = self.chunker.chunks(ir)
        self.assertFalse(next(c for c in children if "乙" in c["body_text"])["overlap_spans"])
        self.verify(ir, parents, children)

    def test_headings_lists_and_metadata(self):
        ir = parse_text("# 第一章\n\n段落甲\n\n- 列表甲\n- 列表乙\n\n## 第二节\n内容乙".encode(), True)
        parents, children = self.chunker.chunks(ir)
        self.assertEqual([p["heading_path"] for p in parents], [["第一章"], ["第一章", "第二节"]])
        self.assertEqual(sum(b["type"] == "list_item" for b in ir["blocks"]), 2)
        self.assertTrue(all(s["page_no"] is None for c in children for s in c["source_spans"]))
        self.verify(ir, parents, children)

    def test_large_metadata_is_not_lost(self):
        ir = parse_text(("# " + "标题" * 300 + "\n\n正文").encode(), True)
        chunker = Chunker(CharacterTokenizer(), "文件名" * 300)
        parents, children = chunker.chunks(ir)
        self.assertEqual(children[0]["heading_path"], ["标题" * 300])
        self.assertLessEqual(children[0]["token_count"], 600)

    def test_repeated_heading_is_still_a_section_boundary(self):
        ir = parse_text(b"# Scope\n\nFirst section.\n\n# Scope\n\nSecond section.", True)
        parents, children = self.chunker.chunks(ir)
        self.assertEqual(len(parents), 2)
        self.assertTrue(all(not p["next_id"] and not p["prev_id"] for p in parents))
        self.verify(ir, parents, children)

    def test_table_rows_wide_cells_are_complete_without_overlap(self):
        data = "| 项目 | 内容 |\n| --- | --- |\n| 甲 | " + "长单元格" * 800 + " |\n| 乙 | 20 |"
        ir = parse_text(data.encode(), True)
        self.assertEqual(len(ir["tables"]), 1)
        self.assertEqual(ir["tables"][0]["cells"][3]["raw_text"], "长单元格" * 800)
        parents, children = self.chunker.chunks(ir)
        self.assertTrue(all(c["kind"] == "row_group" and not c["overlap_spans"] for c in children))
        self.verify(ir, parents, children)

    def test_xhtml_page_source_and_merged_cells(self):
        ir = parse_html(b'<html><head><script>ignore</script></head><body><div class="page"><h1>Title</h1><p>Page one</p></div><div class="page"><table><tr><th colspan="2">Header</th></tr><tr><td>A</td><td>B</td></tr></table></div></body></html>')
        self.assertEqual(ir["tables"][0]["cells"][0]["col_span"], 2)
        self.assertEqual(ir["blocks"][1]["source_spans"][0]["page_no"], 1)
        self.assertEqual(ir["blocks"][-1]["source_spans"][0]["page_no"], 2)
        self.assertNotIn("ignore", str(ir))
        parents, children = self.chunker.chunks(ir)
        self.verify(ir, parents, children)

    def test_wide_table_last_column_keeps_identity_and_full_path(self):
        headers = ["very long column name " * 5 + str(i) for i in range(12)]
        data = ("| " + " | ".join(headers) + " |\n| " + " | ".join(["---"] * 12) +
                " |\n| " + " | ".join([str(i) + "x" * 300 for i in range(12)]) + " |")
        ir = parse_text(data.encode(), True)
        parents, children = self.chunker.chunks(ir)
        last = children[-1]
        self.assertIn("table=t0; rows=1:2; columns=11:12", last["embedding_text"])
        self.assertEqual(last["column_paths"], [[headers[-1]]])
        self.verify(ir, parents, children)

    def test_docling_page_bbox_and_multispan_source(self):
        bbox = SimpleNamespace(l=10, t=80, r=60, b=20, coord_origin=SimpleNamespace(value="BOTTOMLEFT"))
        item = SimpleNamespace(label=SimpleNamespace(value="paragraph"), text="abcdef",
                               prov=[SimpleNamespace(page_no=2, charspan=(0, 3), bbox=bbox),
                                     SimpleNamespace(page_no=3, charspan=(3, 6), bbox=bbox)])
        document = SimpleNamespace(iterate_items=lambda: [(item, 1)],
                                   pages={2: SimpleNamespace(size=SimpleNamespace(width=100, height=100)),
                                          3: SimpleNamespace(size=SimpleNamespace(width=100, height=100))})
        ir = docling_ir(document)
        parents, children = self.chunker.chunks(ir)
        self.assertEqual([s["page_no"] for s in children[0]["source_spans"]], [2, 3])
        self.assertEqual(children[0]["source_spans"][0]["bbox"], [0.1, 0.2, 0.6, 0.8])
        self.verify(ir, parents, children)

    def test_input_validation(self):
        with self.assertRaises(UnicodeError):
            parse_text(b"\xff")
        with self.assertRaises(ValueError):
            parse_text(b"binary\x00content")
        with patch.dict(os.environ, {"TOKENIZER_REVISION": "main"}):
            with self.assertRaises(ValueError):
                settings()
        with patch.dict(os.environ, {"TOKENIZER_REVISION": "a" * 40, "EMBEDDING_MODEL": "other"}):
            with self.assertRaises(ValueError):
                settings()
        archive = io.BytesIO()
        with zipfile.ZipFile(archive, "w", zipfile.ZIP_DEFLATED) as output:
            output.writestr("huge.txt", "a" * 1000)
        with self.assertRaises(ValueError):
            validate_archive(archive.getvalue(), 100)

    def test_scan_with_native_header_still_enables_ocr(self):
        image = {"/Subtype": "/Image", "/Width": 1200, "/Height": 1600}
        page = SimpleNamespace(extract_text=lambda: "A native watermark and page header. " * 4,
                               get=lambda key, default: {"/XObject": {"/Image0": image}}, pdf=None,
                               get_contents=lambda: SimpleNamespace(
                                   operations=[([500, 0, 0, 700, 0, 0], b"cm"), (["/Image0"], b"Do")]))
        self.assertTrue(page_needs_ocr(page))
        page.get = lambda key, default: {}
        page.get_contents = lambda: SimpleNamespace(operations=[])
        self.assertFalse(page_needs_ocr(page))

    def test_excel_formats_route_to_tika_with_zip_limits(self):
        cfg = dict(max_bytes=1024, max_chars=10000, tika_url="http://tika:9998",
                   tokenizer_id="fixture", tokenizer_revision="a" * 40, embedding_model="fixture")
        with patch("worker.load_tokenizer", return_value=CharacterTokenizer()):
            parser = Parser(cfg)
        archive = io.BytesIO()
        with zipfile.ZipFile(archive, "w", zipfile.ZIP_DEFLATED) as output:
            output.writestr("xl/workbook.xml", "<workbook/>")
        xhtml = b"<table><tr><th>Value</th></tr><tr><td>42</td></tr></table>"
        for suffix, data, mime in (
            ("xls", b"\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1", "application/vnd.ms-excel"),
            ("xlsx", archive.getvalue(), "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"),
        ):
            with self.subTest(suffix=suffix), patch.object(parser, "tika", return_value=xhtml) as tika:
                with patch("worker.validate_archive", wraps=validate_archive) as check_zip:
                    result = parser.parse(data, "book." + suffix.upper(), "application/octet-stream")
                tika.assert_called_once_with(data, mime)
                self.assertEqual(result["parser"], "tika-xhtml")
                self.assertEqual(result["ir"]["tables"][0]["cells"][-1]["raw_text"], "42")
                self.assertTrue(result["children"])
                if suffix == "xlsx":
                    check_zip.assert_called_once_with(data, cfg["max_bytes"] * 8)
                else:
                    check_zip.assert_not_called()
        oversized = io.BytesIO()
        with zipfile.ZipFile(oversized, "w", zipfile.ZIP_DEFLATED) as output:
            output.writestr("xl/worksheets/sheet1.xml", "a" * 10000)
        with patch.object(parser, "tika") as tika:
            with self.assertRaisesRegex(ValueError, "archive expansion limit"):
                parser.parse(oversized.getvalue(), "large.xlsx", "application/octet-stream")
            tika.assert_not_called()

    def test_timeout_cleans_runner(self):
        runner = ProcessRunner({"timeout": 0.01})
        closed = []
        runner.connection = SimpleNamespace(
            recv=lambda: (time.sleep(0.2), (True, b"late"))[1],
            close=lambda: closed.append(True))
        started = time.monotonic()
        with self.assertRaises(TimeoutError):
            runner.receive()
        self.assertLess(time.monotonic() - started, 0.1)
        self.assertEqual(closed, [True])
        self.assertIsNone(runner.connection)

    def test_real_byte_bpe_unicode_boundaries(self):
        try:
            from tokenizers import Tokenizer, models, pre_tokenizers, processors, trainers
        except ImportError:
            self.skipTest("optional real-tokenizers test requires tokenizers")
        tokenizer = Tokenizer(models.BPE(unk_token="[UNK]"))
        tokenizer.pre_tokenizer = pre_tokenizers.ByteLevel(add_prefix_space=False)
        trainer = trainers.BpeTrainer(vocab_size=320, special_tokens=["[UNK]", "[BOS]", "[EOS]"],
                                      initial_alphabet=pre_tokenizers.ByteLevel.alphabet())
        tokenizer.train_from_iterator(["中文 English 这是段落 🧪 emoji 12345.\n" * 40], trainer=trainer)
        tokenizer.post_processor = processors.TemplateProcessing(single="[BOS] $A [EOS]",
            special_tokens=[("[BOS]", tokenizer.token_to_id("[BOS]")), ("[EOS]", tokenizer.token_to_id("[EOS]"))])
        self.chunker = Chunker(tokenizer, "实验.md")
        text = "# 中英混合\n\n" + "这是段落 Mixed English 🧪 中文。" * 600 + "\n\n新段落。"
        ir = parse_text(text.encode(), True)
        parents, children = self.chunker.chunks(ir)
        self.assertGreater(len(parents), 1)
        self.assertFalse(any(c["overlap_spans"] for c in children))
        self.verify(ir, parents, children)


class HTTPTests(unittest.TestCase):
    def setUp(self):
        self.server = WorkerHTTPServer(("127.0.0.1", 0), Handler)
        self.server.cfg = {"token": "test-secret", "max_bytes": 1024}
        self.calls = []

        def run(data, filename, content_type):
            self.calls.append((data, filename, content_type))
            return b'{"schema_version":"document-v1"}'

        self.server.runner = SimpleNamespace(run=run)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(2)

    def request(self, name="note.txt", body=b"hello", token="Bearer test-secret", extra=None):
        conn = http.client.HTTPConnection(*self.server.server_address, timeout=2)
        headers = {"X-File-Name": urllib.parse.quote(name), "Authorization": token}
        headers.update(extra or {})
        conn.request("POST", "/parse", body, headers)
        response = conn.getresponse()
        result = response.status, json.loads(response.read())
        conn.close()
        return result

    def test_bytes_contract(self):
        status, _ = self.request("中文.md")
        self.assertEqual(status, 200)
        self.assertEqual(self.calls[0][:2], (b"hello", "中文.md"))

    def test_reject_auth_paths_urls_and_size(self):
        self.assertEqual(self.request(token="wrong")[0], 401)
        for name in ("../secret", "C:\\secret.txt", "https://localhost/file", "bad\nname"):
            self.assertEqual(self.request(name)[0], 422)
        self.assertEqual(self.request(body=b"x" * 1025)[0], 413)
        self.assertEqual(self.calls, [])

    def test_health_responds_while_parse_is_busy(self):
        entered, release, result = threading.Event(), threading.Event(), []

        def run(*_):
            entered.set()
            release.wait(1)
            return b'{"schema_version":"document-v1"}'

        self.server.runner = SimpleNamespace(run=run)
        request = threading.Thread(target=lambda: result.append(self.request()), daemon=True)
        request.start()
        self.assertTrue(entered.wait(1))
        try:
            conn = http.client.HTTPConnection(*self.server.server_address, timeout=0.5)
            conn.request("GET", "/health")
            response = conn.getresponse()
            self.assertEqual(response.status, 200)
            response.read()
            conn.close()
        finally:
            release.set()
            request.join(1)
        self.assertEqual(result[0][0], 200)


if __name__ == "__main__":
    unittest.main()
