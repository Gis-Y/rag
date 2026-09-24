"""Structure-first regressions; the character tokenizer is a test double only."""

import unittest

from chunking import Chunker
from test_worker import CharacterTokenizer
from worker import parse_html, parse_text


class StructuralChunkingRegressions(unittest.TestCase):
    def setUp(self):
        self.chunker = Chunker(CharacterTokenizer())

    def check_chunks(self, ir, parents, children):
        parents_by_id = {parent["chunk_id"]: parent for parent in parents}
        covered = {block["block_id"]: set() for block in ir["blocks"] if block["type"] != "heading"}
        for parent in parents:
            self.assertEqual(parent["token_count"], self.chunker.count(parent["context_prefix"] + parent["body_text"]))
            self.assertLessEqual(parent["token_count"], 2000)
            self.assertFalse(parent["overlap_spans"])
        for child in children:
            self.assertEqual(child["token_count"], self.chunker.count(child["embedding_text"]))
            self.assertEqual(child["embedding_text"], child["context_prefix"] + child["body_text"])
            self.assertLessEqual(child["token_count"], 600)
            self.assertTrue(child["embedding_text"].endswith(child["body_text"]))
            parent = parents_by_id[child["parent_id"]]
            for span in child["source_spans"]:
                covered[span["block_id"]].update(range(span["start"], span["end"]))
                self.assertTrue(any(p["block_id"] == span["block_id"] and p["start"] <= span["start"] and
                                    p["end"] >= span["end"] for p in parent["source_spans"]))
        for block in ir["blocks"]:
            if block["block_id"] in covered:
                self.assertEqual(covered[block["block_id"]], set(range(len(block["text"]))))

    def test_fitting_sentences_are_never_cut_to_fill_the_preceding_chunk(self):
        sentences = ["A" * 100 + "。", "B" * 550 + "。"]
        ir = parse_text("".join(sentences).encode())
        parents, children = self.chunker.chunks(ir)
        self.assertEqual([child["body_text"] for child in children], sentences)
        self.assertTrue(all(not child["overlap_spans"] for child in children))
        self.check_chunks(ir, parents, children)

    def test_quoted_english_sentences_preserve_quotes_without_overlap(self):
        for opening, closing in [('"', '"'), ("'", "'"), ("“", "”"), ("‘", "’")]:
            with self.subTest(opening=opening, closing=closing):
                sentences = [opening + "A" * 300 + "." + closing,
                             " " + opening + "B" * 300 + "." + closing]
                ir = parse_text("".join(sentences).encode())
                parents, children = self.chunker.chunks(ir)
                self.assertEqual([child["body_text"] for child in children], sentences)
                self.assertTrue(all(not child["overlap_spans"] for child in children))
                self.check_chunks(ir, parents, children)

    def test_short_code_lines_have_no_overlap_and_keep_language(self):
        text = "```python\n" + "\n".join(f"print({i})" for i in range(120)) + "\n```"
        ir = parse_text(text.encode(), True)
        parents, children = self.chunker.chunks(ir)
        self.assertGreater(len(children), 1)
        self.assertEqual("".join(child["body_text"] for child in children), text)
        for child in children:
            self.assertEqual(child["kind"], "code")
            self.assertTrue(child["embedding_text"].startswith("language=python\n"))
            self.assertIn("language=python", child["context_prefix"])
            self.assertFalse(child["overlap_spans"])
        self.check_chunks(ir, parents, children)

    def test_unlabelled_code_does_not_treat_the_first_code_line_as_language(self):
        ir = parse_text(("```\n" + "print(1)\n" * 120 + "```").encode(), True)
        parents, children = self.chunker.chunks(ir)
        self.assertTrue(all(child["embedding_text"].startswith("code\n") for child in children))
        self.check_chunks(ir, parents, children)

    def test_only_oversized_code_line_overlaps_and_never_across_parents(self):
        prefix, line = "```python\nprint(0)\n", "x" * 4500 + "\n"
        ir = parse_text((prefix + line + "print(1)\n```").encode(), True)
        parents, children = self.chunker.chunks(ir)
        self.assertGreater(len(parents), 1)
        self.assertTrue(any(child["overlap_spans"] for child in children))
        for parent in parents:
            first = next(child for child in children if child["parent_id"] == parent["chunk_id"])
            self.assertFalse(first["overlap_spans"])
        for child in children:
            self.assertTrue(child["embedding_text"].startswith("language=python\n"))
            for span in child["overlap_spans"]:
                self.assertGreaterEqual(span["start"], len(prefix))
                self.assertLessEqual(span["end"], len(prefix + line))
                self.assertLessEqual(self.chunker.count(ir["blocks"][0]["text"][span["start"]:span["end"]]), 75)
        self.check_chunks(ir, parents, children)

    def test_wide_table_continuations_keep_row_label_caption_and_units(self):
        html = ('<table><caption>预算（单位：万元）</caption><tr><th>Product ID</th>'
                '<th>年度预算（万元）</th></tr><tr><td>SKU-UNIQUE-42</td><td>' +
                'value ' * 170 + '</td></tr></table>')
        ir = parse_html(html.encode())
        parents, children = self.chunker.chunks(ir)
        data = [child for child in children if child.get("row_range") == [1, 2]]
        self.assertGreater(len(data), 1)
        for child in data:
            self.assertIn("SKU-UNIQUE-42", child["embedding_text"])
            self.assertIn("预算（单位：万元）", child["embedding_text"])
            self.assertIn("预算（单位：万元）", child["context_prefix"])
            self.assertIn("SKU-UNIQUE-42", child["context_prefix"])
            self.assertFalse(child["overlap_spans"])
            if child["column_paths"] == [["年度预算（万元）"]]:
                self.assertIn("年度预算（万元）", child["embedding_text"])
        self.check_chunks(ir, parents, children)

    def test_wide_table_explicit_row_headers_survive_column_continuations(self):
        for scope in ("row", "rowgroup"):
            with self.subTest(scope=scope):
                html = ('<table><caption>Budget (USD)</caption><tr><th scope="col">Product</th>'
                        '<th scope="col">Amount (USD)</th></tr><tr><th scope="' + scope +
                        '">SKU-42</th><td>' + 'value ' * 170 + '</td></tr></table>')
                ir = parse_html(html.encode())
                parents, children = self.chunker.chunks(ir)
                data = [child for child in children if child.get("row_range") == [1, 2]
                        and child["column_paths"] == [["Amount (USD)"]]]
                self.assertGreater(len(data), 1)
                for child in data:
                    self.assertIn("row 1: SKU-42", child["context_prefix"])
                    self.assertIn("Amount (USD)", child["context_prefix"])
                    self.assertIn("Budget (USD)", child["context_prefix"])
                    self.assertFalse(child["overlap_spans"])
                self.check_chunks(ir, parents, children)

    def test_required_table_metadata_cannot_be_silently_shortened(self):
        for caption, heading in [("C" * 650 + " (USD)", "Amount"), ("Budget", "H" * 650 + " (USD)")]:
            with self.subTest(caption=caption[:8], heading=heading[:8]):
                html = f"<table><caption>{caption}</caption><tr><th>ID</th><th>{heading}</th></tr><tr><td>SKU</td><td>42</td></tr></table>"
                ir = parse_html(html.encode())
                with self.assertRaisesRegex(ValueError, "metadata"):
                    self.chunker.chunks(ir)


if __name__ == "__main__":
    unittest.main()
