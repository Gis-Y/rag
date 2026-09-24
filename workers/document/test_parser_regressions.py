import io
import unittest
from types import SimpleNamespace
from unittest.mock import Mock

from chunking import Chunker
from test_worker import CharacterTokenizer
from worker import docling_ir, page_needs_ocr, parse_html, parse_text, split_table_row


def pdf_page_with_image(form_depth=0, image_size=200, draw=True, display_size=(500, 700)):
    """Real in-memory PDF with native text and an image wrapped in Form XObjects."""
    from pypdf import PdfReader, PdfWriter
    from pypdf.generic import ArrayObject, DecodedStreamObject, DictionaryObject, NameObject, NumberObject

    writer = PdfWriter()
    page = writer.add_blank_page(width=600, height=800)
    image = DecodedStreamObject()
    image.set_data(bytes(image_size * image_size))
    image.update({NameObject("/Type"): NameObject("/XObject"), NameObject("/Subtype"): NameObject("/Image"),
                  NameObject("/Width"): NumberObject(image_size), NameObject("/Height"): NumberObject(image_size),
                  NameObject("/ColorSpace"): NameObject("/DeviceGray"), NameObject("/BitsPerComponent"): NumberObject(8)})
    rendered = writer._add_object(image)
    for _ in range(form_depth):
        form = DecodedStreamObject()
        form.set_data(b"/Child Do")
        form.update({NameObject("/Type"): NameObject("/XObject"), NameObject("/Subtype"): NameObject("/Form"),
                     NameObject("/BBox"): ArrayObject([NumberObject(n) for n in (0, 0, 1, 1)]),
                     NameObject("/Resources"): DictionaryObject({NameObject("/XObject"): DictionaryObject({NameObject("/Child"): rendered})})})
        rendered = writer._add_object(form)
    font = DictionaryObject({NameObject("/Type"): NameObject("/Font"), NameObject("/Subtype"): NameObject("/Type1"),
                             NameObject("/BaseFont"): NameObject("/Helvetica")})
    page[NameObject("/Resources")] = DictionaryObject({
        NameObject("/Font"): DictionaryObject({NameObject("/F1"): writer._add_object(font)}),
        NameObject("/XObject"): DictionaryObject({NameObject("/Rendered"): rendered})})
    content = DecodedStreamObject()
    draw_image = (f" q {display_size[0]} 0 0 {display_size[1]} 20 20 cm /Rendered Do Q".encode()
                  if draw else b"")
    content.set_data(b"BT /F1 12 Tf 20 780 Td (Native watermark and document header with enough characters.) Tj ET" +
                     draw_image)
    page[NameObject("/Contents")] = writer._add_object(content)
    output = io.BytesIO()
    writer.write(output)
    return PdfReader(io.BytesIO(output.getvalue())).pages[0]


class ParserRegressionTests(unittest.TestCase):
    def test_markdown_empty_edge_cells_keep_their_columns(self):
        header = "| ID | Amount | Currency |\n| --- | --- | --- |\n"
        for row, values in [("||42|USD|", ["", "42", "USD"]),
                            ("|SKU|42||", ["SKU", "42", ""]),
                            ("||42||", ["", "42", ""]),
                            (r"|SKU|42|US\||", ["SKU", "42", "US|"]),
                            (r"SKU|42|US\|", ["SKU", "42", "US|"])]:
            with self.subTest(row=row):
                self.assertEqual(split_table_row(row), values)
                ir = parse_text((header + row).encode(), True)
                self.assertEqual([(cell["column"], cell["raw_text"]) for cell in ir["tables"][0]["cells"]
                                  if cell["row"] == 1], list(enumerate(values)))

    def test_html_pre_keeps_code_indentation_and_trailing_newlines(self):
        code = "    if ready:\n        consume()\n    done()\n" + "    done()\n" * 100 + "\n"
        for opening, closing in [("<pre>", "</pre>"), ("<pre><code>", "</code></pre>")]:
            with self.subTest(opening=opening):
                ir = parse_html((opening + code + closing + "<p>  Normal text  </p>").encode())
                self.assertEqual(ir["blocks"][0]["text"], code)
                self.assertEqual(ir["blocks"][0]["type"], "code")
                self.assertEqual(ir["blocks"][1]["text"], "Normal text")
                compile("def f():\n" + ir["blocks"][0]["text"], "html-code", "exec")
                _, children = Chunker(CharacterTokenizer()).chunks(ir)
                code_chunks = [child for child in children if child["kind"] == "code"]
                self.assertGreater(len(code_chunks), 1)
                self.assertEqual("".join(child["body_text"] for child in code_chunks), code)
                self.assertTrue(all(not child["overlap_spans"] for child in code_chunks))

    def test_pdf_nested_form_images_enable_ocr_with_native_watermarks(self):
        for depth in (0, 1, 4):
            with self.subTest(depth=depth):
                page = pdf_page_with_image(depth)
                self.assertGreater(len(page.extract_text()), 30)
                self.assertTrue(page_needs_ocr(page))
        self.assertTrue(page_needs_ocr(pdf_page_with_image(3, 100)))
        self.assertFalse(page_needs_ocr(pdf_page_with_image(0, 500, display_size=(100, 100))))
        self.assertFalse(page_needs_ocr(pdf_page_with_image(0, 500, draw=False)))

    def test_pdf_resource_limits_cycles_and_failures_are_conservative(self):
        class FakeStream(dict):
            def __init__(self, values=None, operations=()):
                super().__init__(values or {})
                self.operations = list(operations)

        def page_with(resources, names):
            return SimpleNamespace(
                get=lambda key, default: resources if key == "/Resources" else default,
                get_contents=lambda: FakeStream(operations=[([name], b"Do") for name in names]),
                extract_text=Mock(return_value="Native text " * 10), pdf=None)

        small_image = {"/Subtype": "/Image", "/Width": 100, "/Height": 100}
        cycle = FakeStream({"/Subtype": "/Form"}, [(["/Self"], b"Do")])
        cycle["/Resources"] = {"/XObject": {"/Self": cycle}}
        deep = small_image
        for _ in range(18):
            deep = FakeStream({"/Subtype": "/Form", "/Resources": {"/XObject": {"/Child": deep}}},
                              [(["/Child"], b"Do")])
        bad_reference = SimpleNamespace(get_object=Mock(side_effect=ValueError("broken reference")))
        cases = [
            ("cycle", page_with({"/XObject": {"/Cycle": cycle}}, ["/Cycle"])),
            ("depth", page_with({"/XObject": {"/Deep": deep}}, ["/Deep"])),
            ("count", page_with({"/XObject": {f"/Image{i}": small_image for i in range(1025)}},
                                [f"/Image{i}" for i in range(1025)])),
            ("broken reference", page_with({"/XObject": {"/Broken": bad_reference}}, ["/Broken"])),
        ]
        for name, page in cases:
            with self.subTest(name=name):
                self.assertTrue(page_needs_ocr(page))
                page.extract_text.assert_not_called()
        shared = FakeStream({"/Subtype": "/Form", "/Resources": {"/XObject": {"/Image": small_image}}},
                            [(["/Image"], b"Do")])
        page = page_with({"/XObject": {"/One": shared, "/Two": shared}}, ["/One", "/Two"])
        self.assertFalse(page_needs_ocr(page))
        page.extract_text.side_effect = ValueError("broken text stream")
        self.assertTrue(page_needs_ocr(page))

    def test_markdown_fence_requires_a_bare_closing_line(self):
        source = "```python\nprint(1)\n```literal\n# still code\n```\nafter"
        ir = parse_text(source.encode(), True)
        self.assertEqual([(block["type"], block["text"]) for block in ir["blocks"]],
                         [("code", "```python\nprint(1)\n```literal\n# still code\n```"),
                          ("paragraph", "after")])

    def test_docling_source_gaps_are_explicitly_unlocated(self):
        item = SimpleNamespace(label=SimpleNamespace(value="paragraph"), text="abcd",
                               prov=[SimpleNamespace(charspan=(1, 3), page_no=1, bbox=None)])
        document = SimpleNamespace(iterate_items=lambda: [(item, 0)], pages={})
        spans = docling_ir(document)["blocks"][0]["source_spans"]
        self.assertEqual([(span["start"], span["end"], span["page_no"]) for span in spans],
                         [(0, 1, None), (1, 3, 1), (3, 4, None)])

    def test_heading_levels_across_all_parsers(self):
        cases = [
            ([(2, "A"), (2, "B")], [["A"], ["B"]]),
            ([(1, "Root"), (3, "A"), (3, "B")], [["Root"], ["Root", "A"], ["Root", "B"]]),
            ([(2, "A"), (4, "Deep"), (3, "Middle"), (2, "B"), (1, "Root")],
             [["A"], ["A", "Deep"], ["A", "Middle"], ["B"], ["Root"]]),
        ]
        for headings, expected in cases:
            markdown = "\n\n".join("#" * level + " " + title + "\n\nbody" for level, title in headings)
            html = "".join(f"<h{level}>{title}</h{level}><p>body</p>" for level, title in headings)
            items = []
            for level, title in headings:
                items.extend([
                    SimpleNamespace(label=SimpleNamespace(value="section_header"), text=title, level=level, prov=[]),
                    SimpleNamespace(label=SimpleNamespace(value="paragraph"), text="body", prov=[]),
                ])
            document = SimpleNamespace(iterate_items=lambda: [(item, 0) for item in items], pages={})
            for name, ir in [("markdown", parse_text(markdown.encode(), True)),
                             ("html", parse_html(html.encode())), ("docling", docling_ir(document))]:
                with self.subTest(parser=name, headings=headings):
                    self.assertEqual([b["heading_path"] for b in ir["blocks"] if b["type"] == "heading"], expected)
                    self.assertEqual([b["heading_path"] for b in ir["blocks"] if b["type"] == "paragraph"], expected)

    def test_html_caption_keeps_units_without_inventing_values(self):
        caption = "Revenue & cost\n(USD thousands)"
        html = ("<h2>Finance</h2><table><caption>Revenue &amp; <em>cost</em><br>(USD thousands)</caption>"
                "<tr><th>Item</th><th>Value</th></tr><tr><td>A</td><td>42</td></tr></table>"
                "<table><tr><td>Uncaptioned</td></tr></table>")
        ir = parse_html(html.encode())
        table = ir["tables"][0]
        self.assertEqual(table["caption"], caption)
        self.assertEqual(table["cells"][-1]["raw_text"], "42")
        self.assertNotIn("unit", table)
        self.assertEqual(ir["tables"][1]["caption"], "")
        rows = [b for b in ir["blocks"] if b.get("table_id") == table["table_id"]]
        self.assertTrue(rows)
        self.assertTrue(all(b["table_caption"] == caption for b in rows))
        self.assertTrue(all(b["heading_path"] == ["Finance"] for b in rows))
        self.assertNotIn(caption, "\n".join(b["text"] for b in rows))

    def test_docling_caption_references_are_preserved(self):
        cell = SimpleNamespace(start_row_offset_idx=0, start_col_offset_idx=0, row_span=1, col_span=1,
                               text="42", column_header=False, row_header=False)
        title = SimpleNamespace(text="Revenue")
        units = SimpleNamespace(text="(USD thousands)")
        refs = [SimpleNamespace(resolve=lambda doc: title), SimpleNamespace(resolve=lambda doc: units)]
        table = SimpleNamespace(label=SimpleNamespace(value="table"), prov=[], captions=refs,
                                data=SimpleNamespace(table_cells=[cell]))
        document = SimpleNamespace(iterate_items=lambda: [(table, 0)], pages={})
        ir = docling_ir(document)
        self.assertEqual(ir["tables"][0]["caption"], "Revenue\n(USD thousands)")
        self.assertEqual(ir["blocks"][0]["table_caption"], "Revenue\n(USD thousands)")
        self.assertEqual(ir["tables"][0]["cells"][0]["raw_text"], "42")
        self.assertTrue(all(s["page_no"] is None for s in ir["blocks"][0]["source_spans"]))

    def test_html_explicit_header_scopes_keep_row_and_column_roles(self):
        for row_scope in ("row", "rowgroup"):
            for column_scope in ("col", "colgroup"):
                with self.subTest(row_scope=row_scope, column_scope=column_scope):
                    ir = parse_html((f'<table><tr><th scope="{column_scope}">Product</th>'
                                     f'<th scope="{column_scope}">Amount (USD)</th></tr>'
                                     f'<tr><th scope="{row_scope}">SKU-42</th><td>42</td></tr></table>').encode())
                    cells = ir["tables"][0]["cells"]
                    self.assertEqual([(cell["column_header"], cell["row_header"]) for cell in cells],
                                     [(True, False), (True, False), (False, True), (False, False)])
                    self.assertEqual(ir["tables"][0]["column_paths"], [["Product"], ["Amount (USD)"]])


if __name__ == "__main__":
    unittest.main()
