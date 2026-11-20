"""Shared reader of sheetmap and sheetcheck: an .xlsx read as cells, never executed.

Standard library only. Streams each sheet, keeps one rendered string per cell and one kind letter per cell:
  n number   d date or time   t text   x text that looks like a number   b boolean   e error value
Nothing in the workbook is run or fetched: macros (vbaProject.bin) and external links are only counted.
"""

import datetime
from collections import Counter
import re
import zipfile
import xml.etree.ElementTree as ET

MAX_CELLS = 2_000_000      # per sheet; more are not read and the sheet says so
MAX_PART = 1 << 30         # bytes of one XML part that are read
NUMLIKE = re.compile(r"^[+-]?(\d{1,3}(,\d{3})+|\d+)(\.\d+)?%?$|^[+-]?\.\d+%?$")
ESCAPE = re.compile(r"_x([0-9A-Fa-f]{4})_")


def local(tag):
    return tag.rpartition("}")[2]


def attr(el, name):
    for key, value in el.attrib.items():
        if key.rpartition("}")[2] == name:
            return value
    return ""


def col_index(ref):
    n = 0
    for ch in ref:
        if not ch.isalpha():
            break
        n = n * 26 + ord(ch.upper()) - 64
    return n


def col_name(n):
    s = ""
    while n > 0:
        n, rem = divmod(n - 1, 26)
        s = chr(65 + rem) + s
    return s or "?"


def split_ref(ref):
    letters = "".join(ch for ch in ref if ch.isalpha())
    digits = "".join(ch for ch in ref if ch.isdigit())
    return (int(digits) if digits else 0), col_index(letters)


def a1(r, c):
    return f"{col_name(c)}{r}"


def rng(r1, c1, r2, c2):
    if (r1, c1) == (r2, c2):
        return a1(r1, c1)
    return f"{a1(r1, c1)}:{a1(r2, c2)}"


def parse_range(ref):
    if not ref:
        return None
    a, _, b = ref.partition(":")
    r1, c1 = split_ref(a)
    r2, c2 = split_ref(b) if b else (r1, c1)
    if not (r1 and c1 and r2 and c2):
        return None
    return min(r1, r2), min(c1, c2), max(r1, r2), max(c1, c2)


def unescape_x(text):
    if "_x" not in text:
        return text
    return ESCAPE.sub(lambda m: chr(int(m.group(1), 16)), text)


def escape_cell(text):
    """The export's escaping: backslash, tab, CR and LF become \\\\, \\t, \\r and \\n."""
    if "\\" in text or "\t" in text or "\n" in text or "\r" in text:
        text = text.replace("\\", "\\\\").replace("\t", "\\t").replace("\r", "\\r").replace("\n", "\\n")
    return text


def unescape_cell(text):
    if "\\" not in text:
        return text
    out, i = [], 0
    while i < len(text):
        ch = text[i]
        if ch == "\\" and i + 1 < len(text):
            out.append({"t": "\t", "r": "\r", "n": "\n", "\\": "\\"}.get(text[i + 1], "\\" + text[i + 1]))
            i += 2
        else:
            out.append(ch)
            i += 1
    return "".join(out)


def num_text(raw):
    raw = raw.strip()
    try:
        return str(int(raw))
    except ValueError:
        pass
    try:
        f = float(raw)
    except ValueError:
        return raw
    if f != f or f in (float("inf"), float("-inf")):
        return raw
    if f == int(f) and abs(f) < 1e15:
        return str(int(f))
    return format(f, ".15g")


def is_date_format(fmt_id, code):
    if code is None:
        return 14 <= fmt_id <= 22 or 27 <= fmt_id <= 36 or 45 <= fmt_id <= 47 or 50 <= fmt_id <= 58
    code = re.sub(r'"[^"]*"', "", code)
    code = re.sub(r"\[(?![hHmMsS]+\])[^\]]*\]", "", code)
    code = re.sub(r"\\.", "", code)
    code = re.sub(r"_.", "", code)
    return re.search(r"[yYmMdDhHsS]", code) is not None


def date_text(serial, date1904):
    try:
        value = float(serial)
    except ValueError:
        return serial
    if value < 0 or value >= 2958466:
        return num_text(serial)
    days = int(value)
    seconds = int(round((value - days) * 86400))
    if seconds >= 86400:
        days, seconds = days + 1, 0
    clock = f"{seconds // 3600:02d}:{seconds % 3600 // 60:02d}:{seconds % 60:02d}"
    if value < 1 and not date1904:
        return clock
    if date1904:
        day = datetime.date(1904, 1, 1) + datetime.timedelta(days=days)
    elif days == 60:
        return "1900-02-29" + ("" if seconds == 0 else " " + clock)
    elif days < 60:
        day = datetime.date(1899, 12, 31) + datetime.timedelta(days=days)
    else:
        day = datetime.date(1899, 12, 30) + datetime.timedelta(days=days)
    return day.isoformat() + ("" if seconds == 0 else " " + clock)


def si_text(si):
    parts = []
    for child in si:
        name = local(child.tag)
        if name == "t":
            parts.append(child.text or "")
        elif name == "r":
            for t in child:
                if local(t.tag) == "t":
                    parts.append(t.text or "")
    return unescape_x("".join(parts))


class Sheet:
    def __init__(self, index, name, state, part):
        self.index, self.name, self.state, self.part = index, name, state, part
        self.cells = {}      # row -> {col: rendered text}
        self.kinds = {}      # row -> {col: kind letter}
        self.merges = []     # (r1, c1, r2, c2)
        self.formulas = []   # (ref, formula, cached text)
        self.dimension = ""
        self.hidden_rows = 0
        self.truncated = False
        self.error = ""
        self.ncells = 0

    def used(self):
        """(r1, c1, r2, c2) of the cells that hold a value, or None."""
        if not self.cells:
            return None
        rows = self.cells.keys()
        cols = [c for row in self.cells.values() for c in row]
        return min(rows), min(cols), max(rows), max(cols)

    def kind_counts(self):
        counts = {}
        for row in self.kinds.values():
            for k in row.values():
                counts[k] = counts.get(k, 0) + 1
        return counts

    def lines(self):
        """Yield (row, [cell texts]) for rows 1..last, empty rows included, trailing empty cells dropped."""
        used = self.used()
        if not used:
            return
        for r in range(1, used[2] + 1):
            row = self.cells.get(r)
            if not row:
                yield r, []
                continue
            last = max(row)
            yield r, [row.get(c, "") for c in range(1, last + 1)]


class Workbook:
    def __init__(self, path):
        self.path = path
        self.sheets = []
        self.date1904 = False
        self.external_links = 0
        self.macros = False
        self.warnings = []


def _iterparse(zf, name):
    info = zf.getinfo(name)
    if info.file_size > MAX_PART:
        raise ValueError(f"{name} is larger than {MAX_PART} bytes")
    return zf.open(name)


def _shared_strings(zf):
    try:
        stream = _iterparse(zf, "xl/sharedStrings.xml")
    except KeyError:
        return []
    out = []
    try:
        for _, el in ET.iterparse(stream, events=("end",)):
            if local(el.tag) == "si":
                out.append(si_text(el))
                el.clear()
    except ET.ParseError:
        pass
    finally:
        stream.close()
    return out


def _date_styles(zf):
    """The set of cell style indexes whose number format is a date or time."""
    try:
        root = ET.fromstring(zf.read("xl/styles.xml"))
    except (KeyError, ET.ParseError):
        return set()
    codes = {}
    for node in root.iter():
        if local(node.tag) == "numFmt":
            try:
                codes[int(attr(node, "numFmtId"))] = attr(node, "formatCode")
            except ValueError:
                pass
    dates = set()
    for node in root:
        if local(node.tag) == "cellXfs":
            for i, xf in enumerate(x for x in node if local(x.tag) == "xf"):
                try:
                    fid = int(attr(xf, "numFmtId") or 0)
                except ValueError:
                    continue
                if is_date_format(fid, codes.get(fid)):
                    dates.add(i)
    return dates


def _target(target):
    target = target.lstrip("/")
    return target if target.startswith("xl/") else "xl/" + target


def open_workbook(path):
    """Read the workbook's structure and every sheet. Raises OSError, zipfile.BadZipFile, KeyError or ET.ParseError."""
    wb = Workbook(path)
    with zipfile.ZipFile(path) as zf:
        names = zf.namelist()
        wb.macros = any(n.lower().endswith("vbaproject.bin") for n in names)
        wb.external_links = sum(1 for n in names if n.startswith("xl/externalLinks/") and n.endswith(".xml"))
        root = ET.fromstring(zf.read("xl/workbook.xml"))
        rels = ET.fromstring(zf.read("xl/_rels/workbook.xml.rels"))
        targets = {attr(r, "Id"): _target(attr(r, "Target")) for r in rels}
        for node in root.iter():
            if local(node.tag) == "workbookPr":
                wb.date1904 = attr(node, "date1904") in ("1", "true")
        strings = _shared_strings(zf)
        dates = _date_styles(zf)
        i = 0
        for node in root.iter():
            if local(node.tag) != "sheet":
                continue
            i += 1
            sheet = Sheet(i, attr(node, "name") or "(unnamed)", attr(node, "state") or "visible", targets.get(attr(node, "id"), ""))
            try:
                _read_sheet(zf, sheet, strings, dates, wb.date1904)
            except (KeyError, ET.ParseError, ValueError, zipfile.BadZipFile) as err:
                sheet.error = f"unreadable ({err})"
            wb.sheets.append(sheet)
    return wb


def _read_sheet(zf, sheet, strings, dates, date1904):
    stream = _iterparse(zf, sheet.part)
    row_no = 0
    col_no = 0
    try:
        for event, el in ET.iterparse(stream, events=("start", "end")):
            tag = local(el.tag)
            if event == "start":
                if tag == "row":
                    ref = el.attrib.get("r")
                    row_no = int(ref) if ref and ref.isdigit() else row_no + 1
                    col_no = 0
                    if el.attrib.get("hidden") in ("1", "true"):
                        sheet.hidden_rows += 1
                continue
            if tag == "c":
                _read_cell(el, sheet, strings, dates, date1904, row_no, col_no)
                ref = el.attrib.get("r")
                col_no = split_ref(ref)[1] if ref else col_no + 1
                el.clear()
            elif tag == "row":
                el.clear()
            elif tag == "mergeCell":
                box = parse_range(el.attrib.get("ref", ""))
                if box:
                    sheet.merges.append(box)
            elif tag == "dimension":
                sheet.dimension = el.attrib.get("ref", "")
            if sheet.ncells > MAX_CELLS:
                sheet.truncated = True
                break
    finally:
        stream.close()


def _style(el):
    try:
        return int(el.attrib.get("s", "0") or 0)
    except ValueError:
        return 0


def _read_cell(el, sheet, strings, dates, date1904, row_no, col_no):
    if sheet.ncells > MAX_CELLS:
        return
    ref = el.attrib.get("r")
    if ref:
        row, col = split_ref(ref)
        row = row or row_no
    else:
        row, col = row_no, col_no + 1
    if not row or not col:
        return
    t = el.attrib.get("t", "n")
    raw = ""
    formula = None
    inline = ""
    for child in el:
        name = local(child.tag)
        if name == "v":
            raw = child.text or ""
        elif name == "f":
            formula = (child.text or "").strip() or ("(shared formula)" if child.attrib.get("t") == "shared" else "(formula)")
        elif name == "is":
            inline = si_text(child)
    kind = "t"
    if t == "inlineStr":
        text = inline
    elif t == "s":
        try:
            text = strings[int(raw)]
        except (ValueError, IndexError):
            text = ""
    elif t == "str":
        text = unescape_x(raw)
    elif t == "b":
        text, kind = ("TRUE" if raw.strip() == "1" else "FALSE"), "b"
    elif t == "e":
        text, kind = raw, "e"
    elif t == "d":
        text, kind = raw, "d"
    else:
        if raw.strip() == "":
            text = ""
        elif _style(el) in dates:
            text, kind = date_text(raw, date1904), "d"
        else:
            text, kind = num_text(raw), "n"
            try:
                float(raw)
            except ValueError:
                kind = "t"
    if kind == "t" and text and NUMLIKE.match(text.strip()):
        kind = "x"
    if formula is not None:
        sheet.formulas.append((a1(row, col), formula, text))
    if text == "":
        return
    sheet.cells.setdefault(row, {})[col] = text
    sheet.kinds.setdefault(row, {})[col] = kind
    sheet.ncells += 1


class Block:
    def __init__(self, r1, c1, r2, c2):
        self.r1, self.c1, self.r2, self.c2 = r1, c1, r2, c2
        self.cells = 0
        self.counts = {}
        self.covered = 0          # cells with no value that a merged range covers
        self.titles = []          # rows (numbers) that hold a single value above the header
        self.header = []          # header rows (numbers)
        self.header_note = ""
        self.merges = []
        self.sparse_cols = []     # interior columns that hold almost nothing: two tables joined by a stray cell?

    @property
    def rows(self):
        return self.r2 - self.r1 + 1

    @property
    def cols(self):
        return self.c2 - self.c1 + 1

    @property
    def range(self):
        return rng(self.r1, self.c1, self.r2, self.c2)

    def numeric(self):
        return self.counts.get("n", 0) + self.counts.get("d", 0)

    def pct_numeric(self):
        return 100 * self.numeric() // self.cells if self.cells else 0

    def holes(self):
        return self.rows * self.cols - self.cells - self.covered


def _groups(values):
    """Consecutive runs of a sorted list of ints."""
    runs = []
    for v in values:
        if runs and v == runs[-1][-1] + 1:
            runs[-1].append(v)
        else:
            runs.append([v])
    return runs


def _split_rows(rows):
    parts, cur, prev = [], [], None
    for item in rows:
        if prev is not None and item[0] != prev + 1:
            parts.append(cur)
            cur = []
        cur.append(item)
        prev = item[0]
    if cur:
        parts.append(cur)
    return parts


def _split_cols(rows):
    cols = sorted({c for _, cs in rows for c in cs})
    runs = _groups(cols)
    if len(runs) < 2:
        return [rows]
    which = {}
    for gi, run in enumerate(runs):
        for c in run:
            which[c] = gi
    parts = [[] for _ in runs]
    for r, cs in rows:
        per = {}
        for c in cs:
            per.setdefault(which[c], []).append(c)
        for gi, group in per.items():
            parts[gi].append((r, group))
    return parts


def find_blocks(sheet, with_header=True):
    """Rectangles of occupied cells separated by empty rows or columns (a merged range counts as occupied)."""
    occ = {r: set(row) for r, row in sheet.cells.items()}
    for r1, c1, r2, c2 in sheet.merges:
        if (r2 - r1 + 1) * (c2 - c1 + 1) <= 20000:
            for r in range(r1, r2 + 1):
                occ.setdefault(r, set()).update(range(c1, c2 + 1))
        else:
            occ.setdefault(r1, set()).add(c1)
            occ.setdefault(r2, set()).add(c2)
    start = [(r, sorted(cs)) for r, cs in sorted(occ.items())]
    blocks = []
    stack = [start] if start else []
    while stack:
        rows = stack.pop()
        parts = _split_rows(rows)
        if len(parts) == 1:
            parts = _split_cols(rows)
        if len(parts) > 1:
            stack.extend(reversed(parts))
            continue
        cs = [c for _, cols in rows for c in cols]
        block = Block(rows[0][0], min(cs), rows[-1][0], max(cs))
        for r, cols in rows:
            kinds = sheet.kinds.get(r, {})
            for c in cols:
                k = kinds.get(c)
                if k is None:
                    block.covered += 1
                else:
                    block.cells += 1
                    block.counts[k] = block.counts.get(k, 0) + 1
        blocks.append(block)
    blocks.sort(key=lambda b: (b.r1, b.c1))
    for block in blocks:
        block.merges = [m for m in sheet.merges if block.r1 <= m[0] <= block.r2 and block.c1 <= m[1] <= block.c2 and m[:2] != m[2:]]
        if with_header:
            _header(sheet, block)
            _sparse(sheet, block)
    return blocks


def _sparse(sheet, block):
    if block.rows < 8 or block.cols < 3:
        return
    fill = Counter()
    for r in range(block.r1, block.r2 + 1):
        for c in sheet.cells.get(r, ()):
            if block.c1 < c < block.c2:
                fill[c] += 1
    limit = max(1, block.rows // 30)
    block.sparse_cols = [(c, fill[c]) for c in range(block.c1 + 1, block.c2) if 0 < fill[c] <= limit]


def _header(sheet, block):
    rows = []
    for r in range(block.r1, block.r2 + 1):
        cells = {c: v for c, v in sheet.cells.get(r, {}).items() if block.c1 <= c <= block.c2}
        kinds = sheet.kinds.get(r, {})
        rows.append((r, cells, sum(1 for c in cells if kinds.get(c) in ("n", "d"))))
    i = 0
    if block.cols >= 3 and len(rows) >= 3:
        while i < 2 and i < len(rows) - 1 and len(rows[i][1]) == 1:
            block.titles.append(rows[i][0])
            i += 1
    head = []
    j = i
    while j < len(rows) and len(head) < 3 and rows[j][2] == 0 and len(rows[j][1]) >= 2:
        head.append(rows[j][0])
        j += 1
    if j < len(rows) and head:
        block.header = head
        body_numbers = sum(r[2] for r in rows[j:])
        block.header_note = "" if body_numbers else "guess: no numbers below"
    elif not head and j < len(rows) and block.cols >= 2 and rows[j][2] == 0 and len(rows[j][1]) >= 2 and len(rows) - j >= 2:
        block.header = [rows[j][0]]
        block.header_note = "guess: text only"
    elif block.cols == 1:
        block.header_note = "one column"
    else:
        block.header_note = "none found"


def clip(text, n=14):
    text = " ".join(text.split())
    return text if len(text) <= n else text[: n - 1] + "…"


def header_text(sheet, block, limit=8):
    out = []
    for r in block.header:
        row = sheet.cells.get(r, {})
        vals = [clip(row[c]) for c in sorted(row) if block.c1 <= c <= block.c2]
        more = " | …" if len(vals) > limit else ""
        out.append(f"r{r}: " + " | ".join(vals[:limit]) + more)
    return "; ".join(out)
