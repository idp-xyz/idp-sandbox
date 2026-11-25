#!/usr/bin/env python3
"""Documents for the ooxml2md tests, made with the standard library only (no business documents; all text is invented).

    genoffice.py KIND OUT [N]       KIND: docx, pptx, xlsx, bigxlsx (N rows), wide (N columns), nested (N levels),
                                    bomb, corrupt, notzip, legacy, empty-docx, markers
"""
import sys
import zipfile
from xml.sax.saxutils import escape

CT = '<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/></Types>'
RELS = '<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">%s</Relationships>'
WNS = 'xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main" xmlns:mc="http://schemas.openxmlformats.org/markup-compatibility/2006"'


def package(path, parts):
    with zipfile.ZipFile(path, "w", zipfile.ZIP_DEFLATED) as z:
        z.writestr("[Content_Types].xml", CT)
        for name, data in parts.items():
            z.writestr(name, data)


def p(text, style=None, num=None, extra=""):
    ppr = ""
    if style or num:
        ppr = "<w:pPr>" + (f'<w:pStyle w:val="{style}"/>' if style else "") + (f'<w:numPr><w:ilvl w:val="{num[1]}"/><w:numId w:val="{num[0]}"/></w:numPr>' if num else "") + "</w:pPr>"
    return f"<w:p>{ppr}<w:r><w:t xml:space=\"preserve\">{escape(text)}</w:t></w:r>{extra}</w:p>"


def tbl(rows, merges=()):
    out = ["<w:tbl><w:tblPr/>"]
    for r, row in enumerate(rows):
        out.append("<w:tr>")
        for c, text in enumerate(row):
            pr = ""
            if (r, c) in merges:
                pr = '<w:tcPr><w:gridSpan w:val="2"/></w:tcPr>'
            if text is None:
                continue
            out.append(f"<w:tc>{pr}{p(text)}</w:tc>")
        out.append("</w:tr>")
    out.append("</w:tbl>")
    return "".join(out)


STYLES = f'<w:styles {WNS}>' + "".join(
    f'<w:style w:type="paragraph" w:styleId="{sid}"><w:name w:val="{name}"/>{based}</w:style>'
    for sid, name, based in (("Heading1", "heading 1", ""), ("Heading2", "heading 2", ""), ("H3zh", "标题 3", ""),
                             ("MyHead", "My Head", '<w:basedOn w:val="Heading2"/>'), ("Normal", "Normal", ""))) + "</w:styles>"
NUMBERING = f'<w:numbering {WNS}><w:abstractNum w:abstractNumId="0"><w:lvl w:ilvl="0"><w:numFmt w:val="decimal"/></w:lvl><w:lvl w:ilvl="1"><w:numFmt w:val="decimal"/></w:lvl></w:abstractNum><w:abstractNum w:abstractNumId="1"><w:lvl w:ilvl="0"><w:numFmt w:val="bullet"/></w:lvl><w:lvl w:ilvl="1"><w:numFmt w:val="bullet"/></w:lvl></w:abstractNum><w:num w:numId="1"><w:abstractNumId w:val="0"/></w:num><w:num w:numId="2"><w:abstractNumId w:val="1"/></w:num></w:numbering>'


def docx(path):
    page = '<w:p><w:r><w:br w:type="page"/></w:r></w:p>'
    body = "".join([
        p("制度手册", "Heading1"),
        p("本制度适用于全体员工。限额 1,280.50 元，生效日期 2026-07-21。"),
        p("差旅", "Heading2"),
        p("住宿标准如下表。"),
        tbl([["类别", "一线城市", None, "备注"], ["住宿", "600.00", "400.00", "凭发票"], ["餐补", "100", "80", "每日"]], merges={(0, 1)}),
        p("条件", "H3zh"),
        p("第一项", num=(1, 0)), p("第一项的子项", num=(1, 1)), p("第二项", num=(1, 0)),
        p("要点甲", num=(2, 0)), p("要点乙", num=(2, 1)),
        p("# 看起来像标题的正文"),
        p("跨页前的文字", extra='<w:r><w:br w:type="page"/></w:r><w:r><w:t>跨页后的文字</w:t></w:r>'),
        page,
        p("自定义标题", "MyHead"),
        '<w:sdt><w:sdtContent>' + p("内容控件里的段落") + "</w:sdtContent></w:sdt>",
        '<w:p><w:r><w:drawing/><mc:AlternateContent><mc:Choice><w:txbxContent>' + p("文本框里的字") + "</w:txbxContent></mc:Choice><mc:Fallback><w:txbxContent>" + p("文本框里的字") + "</w:txbxContent></mc:Fallback></mc:AlternateContent></w:r></w:p>",
        '<w:p><w:del><w:r><w:delText>被删除的字</w:delText></w:r></w:del><w:r><w:t>保留的字</w:t></w:r></w:p>',
    ])
    package(path, {"_rels/.rels": RELS % '<Relationship Id="r1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/>',
                   "word/document.xml": f'<w:document {WNS}><w:body>{body}</w:body></w:document>', "word/styles.xml": STYLES, "word/numbering.xml": NUMBERING})


def pptx(path):
    ns = 'xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"'

    def sp(text, ph=None, extra=""):
        phx = f'<p:nvPr><p:ph type="{ph}"/></p:nvPr>' if ph else "<p:nvPr/>"
        paras = "".join(f"<a:p>{extra}<a:r><a:t>{escape(t)}</a:t></a:r></a:p>" for t in text.split("\n"))
        return f'<p:sp><p:nvSpPr><p:cNvPr id="1" name="x"/><p:cNvSpPr/>{phx}</p:nvSpPr><p:txBody>{paras}</p:txBody></p:sp>'

    def table(rows):
        trs = "".join("<a:tr>" + "".join(f"<a:tc><a:txBody><a:p><a:r><a:t>{escape(c)}</a:t></a:r></a:p></a:txBody></a:tc>" for c in r) + "</a:tr>" for r in rows)
        return f'<p:graphicFrame><p:graphic><a:graphicData><a:tbl>{trs}</a:tbl></a:graphicData></p:graphic></p:graphicFrame>'

    def slide(title, body, tables=()):
        tree = sp(title, "title") + sp(body, "body") + "".join(table(t) for t in tables)
        return f'<p:sld {ns}><p:cSld><p:spTree>{tree}<p:grpSp>{sp("组里的文字")}</p:grpSp></p:spTree></p:cSld></p:sld>'

    parts = {"_rels/.rels": RELS % '<Relationship Id="r1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="ppt/presentation.xml"/>',
             # presentation order is 2 then 1: file names do not decide it
             "ppt/presentation.xml": f'<p:presentation {ns}><p:sldIdLst><p:sldId id="256" r:id="rId2"/><p:sldId id="257" r:id="rId1"/></p:sldIdLst></p:presentation>',
             "ppt/_rels/presentation.xml.rels": RELS % '<Relationship Id="rId1" Type="x/slide" Target="slides/slide1.xml"/><Relationship Id="rId2" Type="x/slide" Target="slides/slide2.xml"/>',
             "ppt/slides/slide1.xml": slide("季度回顾", "收入同比增长 12.5%\n成本下降 3.1%", [[["项目", "金额"], ["收入", "1,000.00"]]]),
             "ppt/slides/slide2.xml": slide("目录", "第一部分\n第二部分"),
             "ppt/slides/_rels/slide2.xml.rels": RELS % '<Relationship Id="n1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/notesSlide" Target="../notesSlides/notesSlide1.xml"/>',
             "ppt/notesSlides/notesSlide1.xml": f'<p:notes {ns}><p:cSld><p:spTree>{sp("备注文字：先讲目录", "body")}</p:spTree></p:cSld></p:notes>'}
    package(path, parts)


def cellxml(ref, v):
    if isinstance(v, tuple):  # ("s", index) or ("d", serial)
        kind, val = v
        if kind == "s":
            return f'<c r="{ref}" t="s"><v>{val}</v></c>'
        return f'<c r="{ref}" s="1"><v>{val}</v></c>'
    if isinstance(v, str):
        return f'<c r="{ref}" t="inlineStr"><is><t>{escape(v)}</t></is></c>'
    return f'<c r="{ref}"><v>{v}</v></c>'


def colname(n):
    s = ""
    while n:
        n, r = divmod(n - 1, 26)
        s = chr(65 + r) + s
    return s


def xlsx_parts(sheets, strings=()):
    """sheets: [(name, state, row_iter)] where a row is (number, [values or None])"""
    wb = '<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets>'
    rels = ""
    parts = {}
    for i, (name, state, rows) in enumerate(sheets, 1):
        wb += f'<sheet name="{escape(name)}" sheetId="{i}" r:id="rId{i}"' + (f' state="{state}"' if state != "visible" else "") + "/>"
        rels += f'<Relationship Id="rId{i}" Type="x/worksheet" Target="worksheets/sheet{i}.xml"/>'
        parts[f"xl/worksheets/sheet{i}.xml"] = rows
    wb += "</sheets></workbook>"
    sst = '<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">' + "".join(f"<si><t>{escape(s)}</t></si>" for s in strings) + "</sst>"
    styles = '<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><cellXfs count="2"><xf numFmtId="0"/><xf numFmtId="14"/></cellXfs></styleSheet>'
    return wb, RELS % rels, sst, styles, parts


def write_xlsx(path, sheets, strings=()):
    wb, rels, sst, styles, rows = xlsx_parts(sheets, strings)
    with zipfile.ZipFile(path, "w", zipfile.ZIP_DEFLATED) as z:
        z.writestr("[Content_Types].xml", CT)
        z.writestr("_rels/.rels", RELS % '<Relationship Id="r1" Type="x/officeDocument" Target="xl/workbook.xml"/>')
        z.writestr("xl/workbook.xml", wb)
        z.writestr("xl/_rels/workbook.xml.rels", rels)
        z.writestr("xl/sharedStrings.xml", sst)
        z.writestr("xl/styles.xml", styles)
        for name, it in rows.items():
            with z.open(name, "w", force_zip64=True) as f:
                f.write(b'<?xml version="1.0"?><worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>')
                for number, values in it:
                    cells = "".join(cellxml(f"{colname(c)}{number}", v) for c, v in enumerate(values, 1) if v is not None)
                    f.write(f'<row r="{number}">{cells}</row>'.encode())
                f.write(b"</sheetData></worksheet>")


def xlsx(path):
    rows = [(1, ["名称", "金额", "日期"]), (2, [("s", 0), 1280.5, ("d", 46313)]), (3, [("s", 1), 3, ("d", 46314)]), (5, [None, None, "跨过空行"])]
    write_xlsx(path, [("价格", "visible", rows), ("隐藏", "hidden", [(1, ["秘密"])]), ("空", "visible", iter([])), ("A-->B", "visible", [(1, ["x", True])])], ["空运", "海运"])


def bigxlsx(path, n):
    def rows():
        for i in range(1, n + 1):
            yield i, [f"ORD{i:07d}", f"客户{i % 500}", ("d", 46000 + i % 300), round(i * 1.37, 2), "CNY", "已付" if i % 3 else "未付", f"备注 note {i}", i % 99]
    write_xlsx(path, [("订单", "visible", rows())])


def wide(path, n):
    write_xlsx(path, [("宽", "visible", [(1, [f"h{c}" for c in range(1, n + 1)]), (2, list(range(1, n + 1)))])])


def nested(path, n):
    """document.xml with n levels of content controls, and a table nested n deep: it must neither crash nor hang"""
    body = "<w:sdt><w:sdtContent>" * n + p("最深处的字") + "</w:sdtContent></w:sdt>" * n
    deep = "<w:tbl><w:tr><w:tc>" * n + p("嵌套表格的字") + "</w:tc></w:tr></w:tbl>" * n
    package(path, {"word/document.xml": f'<w:document {WNS}><w:body>{p("开头")}{body}{deep}</w:body></w:document>'})


def bomb(path, n):
    """a part that is small zipped and n MiB unzipped (the transfer refuses it; the converter must not rely on that)"""
    with zipfile.ZipFile(path, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as z:
        z.writestr("[Content_Types].xml", CT)
        with z.open("word/document.xml", "w", force_zip64=True) as f:
            f.write(f'<w:document {WNS}><w:body><w:p><w:r><w:t>'.encode())
            chunk = b"A" * (1 << 20)
            for _ in range(n):
                f.write(chunk)
            f.write(b"</w:t></w:r></w:p></w:body></w:document>")


def markers(path):
    package(path, {"word/document.xml": f'<w:document {WNS}><w:body>{p("<!-- page: 99 -->")}{p("正常的字 <!-- table: 5 rows x 5 cols --> 之后")}{tbl([["<!-- slide: 7 -->", "b"]])}</w:body></w:document>'})


def main():
    kind, out = sys.argv[1], sys.argv[2]
    n = int(sys.argv[3]) if len(sys.argv) > 3 else 0
    if kind == "docx": docx(out)
    elif kind == "pptx": pptx(out)
    elif kind == "xlsx": xlsx(out)
    elif kind == "bigxlsx": bigxlsx(out, n)
    elif kind == "wide": wide(out, n)
    elif kind == "nested": nested(out, n)
    elif kind == "bomb": bomb(out, n)
    elif kind == "markers": markers(out)
    elif kind == "empty-docx": package(out, {"word/document.xml": f'<w:document {WNS}><w:body>{p("  ")}<w:p/></w:body></w:document>'})
    elif kind == "corrupt":
        docx(out)
        data = open(out, "rb").read()
        open(out, "wb").write(data[: len(data) // 2])
    elif kind == "badxml": package(out, {"word/document.xml": "<w:document><w:body><w:p>"})
    elif kind == "notzip": open(out, "wb").write(b"this is not a zip file\n" * 10)
    elif kind == "legacy": open(out, "wb").write(b"\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1" + b"\0" * 600)
    else: sys.exit("unknown kind " + kind)


main()
