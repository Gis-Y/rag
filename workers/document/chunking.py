"""Structure-first parent/child chunks. Offsets always address Unicode IR text."""

import re

CHUNKER_VERSION = "structure-parent-child-v2"
CHILD_MAX, CHILD_TARGET = 600, 450
PARENT_MAX, PARENT_TARGET = 2000, 1500
OVERLAP = 75


class Chunker:
    def __init__(self, tokenizer, filename=""):
        self.tokenizer = tokenizer
        self.filename = filename
        self.table_paths = {}
        self.tables = {}
        self.row_labels = {}

    def count(self, text):
        return len(self.tokenizer.encode(text, add_special_tokens=True).ids)

    def prefix(self, heading, table_header=""):
        # Metadata may be shortened in the embedding representation, never in IR.
        prefix = " / ".join([self.filename, *heading]).strip(" / ")
        if table_header:
            prefix += "\n" + table_header
        while prefix and self.count(prefix + "\n\n") > 96:
            prefix = prefix[: max(0, len(prefix) * 3 // 4)]
        return prefix + "\n\n" if prefix else ""

    def fit_end(self, text, start, end, prefix, limit):
        """Find a verified fitting original-character boundary; never decode IDs."""
        if self.count(prefix + text[start:end]) <= limit:
            return end
        if self.count(prefix) >= limit:
            raise ValueError("required chunk metadata leaves no room for source text")
        # Offsets avoid splitting inside a UTF-8 byte token. Binary search alone
        # is not mathematically monotone for BPE, so always verify the candidate.
        encoding = self.tokenizer.encode(text[start:end], add_special_tokens=False)
        candidates = sorted({start + b for _, b in encoding.offsets if b > 0})
        lo, hi, best = 0, len(candidates) - 1, start
        while lo <= hi:
            mid = (lo + hi) // 2
            candidate = candidates[mid]
            if self.count(prefix + text[start:candidate]) <= limit:
                best, lo = candidate, mid + 1
            else:
                hi = mid - 1
        if best == start:
            # A huge single token/unknown token may have no interior offset.
            lo, hi = start + 1, end
            while lo <= hi:
                mid = (lo + hi) // 2
                if self.count(prefix + text[start:mid]) <= limit:
                    best, lo = mid, mid + 1
                else:
                    hi = mid - 1
        if best <= start or self.count(prefix + text[start:best]) > limit:
            raise ValueError("one source character exceeds token budget")
        return best

    def spans(self, block, start, end):
        spans = []
        for origin in block.get("source_spans", []):
            a, b = max(start, origin["start"]), min(end, origin["end"])
            if a < b:
                spans.append({**origin, "block_id": block["block_id"], "start": a, "end": b})
        # Preserve unlocated ranges too; do not invent a page or bounding box.
        covered = sorted((s["start"], s["end"]) for s in spans)
        cursor = start
        for a, b in covered:
            if cursor < a:
                spans.append(dict(block_id=block["block_id"], page_no=None, bbox=None, start=cursor, end=a))
            cursor = max(cursor, b)
        if cursor < end:
            spans.append(dict(block_id=block["block_id"], page_no=None, bbox=None, start=cursor, end=end))
        return sorted(spans, key=lambda s: (s["start"], s["end"]))

    def body(self, pieces):
        parts, previous = [], None
        for block, start, end in pieces:
            if previous is not None and not (previous[0] == block["block_id"] and previous[1] == start):
                parts.append("\n\n")
            parts.append(block["text"][start:end])
            previous = block["block_id"], end
        return "".join(parts)

    def piece_prefix(self, piece):
        return self.chunk_prefix([piece])

    @staticmethod
    def columns(piece):
        block, start, end = piece
        return [i for i, (a, z) in enumerate(block.get("cell_ranges", [])) if a < end and start < z]

    def chunk_prefix(self, pieces):
        block = pieces[0][0]
        metadata = self.prefix(block.get("heading_path", []))
        if not block.get("table_id"):
            if block["type"] == "code":
                language = block.get("language", "")
                if not language:
                    fence = re.match(r"^[ \t]*(?:`{3,}|~{3,})[ \t]*([^\s`~]+)", block["text"])
                    language = fence[1] if fence else ""
                return (f"language={language}\n" if language else "code\n") + metadata
            return metadata
        rows = [b["row_index"] for b, _, _ in pieces]
        columns = [column for piece in pieces for column in self.columns(piece)]
        column_range = f"{min(columns)}:{max(columns) + 1}" if columns else "0:0"
        # Coordinates are mandatory and never shortened with display metadata.
        identity = f"table={block['table_id']}; rows={min(rows)}:{max(rows) + 1}; columns={column_range}\n"
        headings = self.table_paths.get(block["table_id"], [])
        labels = " | ".join(" / ".join(headings[c]) for c in sorted(set(columns)) if c < len(headings))
        caption = self.tables.get(block["table_id"], {}).get("caption") or block.get("table_caption", "")
        row_labels = self.row_labels.get(block["table_id"], {})
        row_identity = "\n".join(f"row {r}: {' | '.join(row_labels[r])}" for r in sorted(set(rows)) if r in row_labels)
        # Caption, column units and row identifiers are required evidence context.
        # Never shorten these: split by rows/columns, or explicitly reject an
        # indivisible prefix that cannot fit beside source text.
        required = [value for value in (caption, labels, row_identity) if value]
        return identity + metadata + ("\n".join(required) + "\n\n" if required else "")

    def render(self, pieces):
        return self.chunk_prefix(pieces) + self.body(pieces)

    def atomize(self, block, limit):
        text = block["text"]
        prefix = self.piece_prefix((block, 0, len(text)))
        if self.count(prefix + text) <= limit:
            return [(block, 0, len(text))]
        out = []
        for _, start, end in self.structural_units((block, 0, len(text))):
            while start < end:
                prefix = self.piece_prefix((block, start, end))
                stop = self.fit_end(text, start, end, prefix, limit)
                out.append((block, start, stop))
                start = stop
        return out

    def structural_units(self, piece):
        """Subdivide only an oversized unit, preserving every original character."""
        block, start, end = piece
        if block.get("table_id"):
            return [(block, max(a, start), min(z, end))
                    for a, z in block.get("cell_ranges") or [(start, end)]
                    if max(a, start) < min(z, end)]
        # Code lines are structures, not sentences; a semicolon inside a line
        # must not enable a different overlap scope.
        pattern = r"\n" if block["type"] == "code" else r"[。！？!?；;]+[”’\"'」』）》）]*|\.[”’\"'」』）》）]*(?=\s|$)|\n"
        boundaries = [start + match.end() for match in re.finditer(pattern, block["text"][start:end])]
        units, cursor = [], start
        for stop in [*boundaries, end]:
            if stop > cursor and block["text"][cursor:stop].strip():
                units.append((block, cursor, stop))
                cursor = stop
        # Attach trailing whitespace to the preceding structure instead of
        # emitting an empty-content child. Leading whitespace stays on the next.
        if units and cursor < end:
            units[-1] = (block, units[-1][1], end)
        return units or [piece]

    def pack(self, pieces, limit, target):
        group = []
        for piece in pieces:
            if group:
                if (self.count(self.render(group)) >= target or
                        self.count(self.render(group + [piece])) > limit):
                    yield group
                    group = []
            group.append(piece)
        if group:
            yield group

    def make_chunk(self, pieces, chunk_id, parent_id="", overlap=None):
        body = self.body(pieces)
        first = pieces[0][0]
        prefix = self.chunk_prefix(pieces)
        spans = [s for b, a, z in pieces for s in self.spans(b, a, z)]
        table_id = first.get("table_id", "")
        chunk = dict(chunk_id=chunk_id, parent_id=parent_id,
                     kind="row_group" if table_id else "code" if first["type"] == "code" else "text", body_text=body,
                     context_prefix=prefix,
                     embedding_text=prefix + body if parent_id else "",
                     heading_path=first.get("heading_path", []), source_spans=spans,
                     overlap_spans=overlap or [], token_count=self.count(prefix + body),
                     block_ids=list(dict.fromkeys(b["block_id"] for b, _, _ in pieces)),
                     prev_id="", next_id="")
        if table_id:
            chunk.update(table_id=table_id, row_range=[min(b["row_index"] for b, _, _ in pieces),
                                                      max(b["row_index"] for b, _, _ in pieces) + 1])
            paths = self.table_paths.get(table_id, [])
            columns = sorted({column for piece in pieces for column in self.columns(piece)})
            chunk["column_paths"] = [paths[c] if c < len(paths) else [f"column {c}"] for c in columns]
        if chunk["token_count"] > (CHILD_MAX if parent_id else PARENT_MAX):
            raise ValueError("required chunk metadata and source exceed token budget")
        return chunk

    def split_child(self, piece):
        block = piece[0]
        # An entire <=600-token structure is immutable, including exactly 600.
        if self.count(self.render([piece])) <= CHILD_MAX:
            yield [piece], []
            return
        pending = []
        for unit in self.structural_units(piece):
            if self.count(self.render([unit])) > CHILD_MAX:
                if pending:
                    yield pending, []
                    pending = []
                yield from self.split_token_unit(unit, overlap=not block.get("table_id"))
            else:
                if pending and (self.count(self.render(pending)) >= CHILD_TARGET or
                                self.count(self.render(pending + [unit])) > CHILD_MAX):
                    yield pending, []
                    pending = []
                pending.append(unit)
        if pending:
            yield pending, []

    def split_token_unit(self, piece, overlap):
        """Only an oversized sentence/code line/cell reaches token fallback."""
        block, start, end = piece
        text = block["text"]
        prefix = self.piece_prefix(piece)
        cursor, previous_end = start, start
        while cursor < end:
            stop = self.fit_end(text, cursor, end, prefix, CHILD_MAX)
            # If overlap leaves no new character, remove it instead of looping.
            if stop <= previous_end:
                cursor = previous_end
                stop = self.fit_end(text, cursor, end, prefix, CHILD_MAX)
            overlap_spans = self.spans(block, cursor, previous_end) if cursor < previous_end else []
            yield [(block, cursor, stop)], overlap_spans
            if stop == end:
                break
            previous_end = stop
            if not overlap:
                cursor = stop
                continue
            encoding = self.tokenizer.encode(text[cursor:stop], add_special_tokens=False)
            offsets = encoding.offsets
            overlap_start = cursor + offsets[max(0, len(offsets) - OVERLAP)][0] if offsets else stop
            while overlap_start < stop and self.count(text[overlap_start:stop]) > OVERLAP:
                overlap_start += 1
            cursor = max(cursor + 1, overlap_start)

    def chunks(self, ir):
        self.tables = {table["table_id"]: table for table in ir.get("tables", [])}
        self.table_paths = {table["table_id"]: table.get("column_paths", []) for table in ir.get("tables", [])}
        self.row_labels = {key: self.table_row_labels(table) for key, table in self.tables.items()}
        parents, children = [], []
        sections, section, key = [], [], None
        for block in ir["blocks"]:
            if block["type"] == "heading":
                if section:
                    sections.append(section)
                    section = []
                key = None
                continue
            if not block["text"]:
                continue
            new_key = (tuple(block.get("heading_path", [])), block.get("table_id", ""),
                       block["block_id"] if block["type"] == "code" else "")
            if section and new_key != key:
                sections.append(section)
                section = []
            key = new_key
            section.extend(self.atomize(block, PARENT_MAX))
        if section:
            sections.append(section)
        for pieces in sections:
            section_parents = []
            for group in self.pack(pieces, PARENT_MAX, PARENT_TARGET):
                parent = self.make_chunk(group, f"p{len(parents)}")
                parents.append(parent)
                section_parents.append(parent)
                pending, parent_children = [], []

                def emit(items, overlap=None):
                    child = self.make_chunk(items, f"c{len(children)}", parent["chunk_id"], overlap)
                    children.append(child)
                    parent_children.append(child)

                for piece in group:
                    if self.count(self.render([piece])) > CHILD_MAX:
                        if pending:
                            emit(pending)
                            pending = []
                        for items, overlap in self.split_child(piece):
                            emit(items, overlap)
                    else:
                        if pending and (self.count(self.render(pending)) >= CHILD_TARGET or
                                        self.count(self.render(pending + [piece])) > CHILD_MAX):
                            emit(pending)
                            pending = []
                        pending.append(piece)
                if pending:
                    emit(pending)
                self.link(parent_children)
            self.link(section_parents)
        return parents, children

    @staticmethod
    def table_row_labels(table):
        """Keep declared row headers, or the first visible data-cell value."""
        labels, rows = {}, {}
        cells = table.get("cells", [])
        header_rows = {cell["row"] for cell in cells if cell.get("column_header")}
        for cell in cells:
            if not cell.get("raw_text", "").strip() or cell.get("column_header"):
                continue
            rows.setdefault(cell["row"], []).append(cell)
            if cell.get("row_header"):
                for row in range(cell["row"], cell["row"] + cell.get("row_span", 1)):
                    labels.setdefault(row, []).append(cell["raw_text"])
        for row, values in rows.items():
            if row not in labels and row not in header_rows:
                # ponytail: a visible row locator is not a declared business key;
                # use explicit row_header metadata when a parser provides it.
                labels[row] = [min(values, key=lambda cell: cell["column"])["raw_text"]]
        return labels

    @staticmethod
    def link(chunks):
        for i, chunk in enumerate(chunks):
            chunk["prev_id"] = chunks[i - 1]["chunk_id"] if i else ""
            chunk["next_id"] = chunks[i + 1]["chunk_id"] if i + 1 < len(chunks) else ""
