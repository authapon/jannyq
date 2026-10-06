#!/usr/bin/env python3
"""Regenerates the fixtures used by the attach tests (needs reportlab and Pillow).

  python3 generate.py

The files are committed so that the tests do not depend on Python.
"""
import io
import struct

from PIL import Image, ImageDraw, ImageFont
from reportlab.lib.pagesizes import A4
from reportlab.pdfbase import pdfmetrics
from reportlab.pdfbase.ttfonts import TTFont
from reportlab.pdfgen import canvas
from reportlab.lib.utils import ImageReader

LATIN = "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf"
THAI = "/usr/share/fonts/truetype/tlwg/Garuda.ttf"
pdfmetrics.registerFont(TTFont("Latin", LATIN))
pdfmetrics.registerFont(TTFont("Thai", THAI))


def text_pdf(path, pages, font, password=None):
    kw = {}
    if password:
        from reportlab.lib import pdfencrypt
        kw["encrypt"] = pdfencrypt.StandardEncryption(password, canPrint=0)
    c = canvas.Canvas(path, pagesize=A4, **kw)
    for lines in pages:
        c.setFont(font, 14)
        y = 780
        for line in lines:
            c.drawString(60, y, line)
            y -= 24
        c.showPage()
    c.save()


def scanned_pdf(path, lines, font_path):
    font = ImageFont.truetype(font_path, 44)
    img = Image.new("L", (1240, 600), 255)
    d = ImageDraw.Draw(img)
    y = 60
    for line in lines:
        d.text((60, y), line, font=font, fill=0)
        y += 90
    c = canvas.Canvas(path, pagesize=A4)
    c.drawImage(ImageReader(img.convert("RGB")), 30, 450, width=535, height=258)
    c.showPage()
    c.save()


text_pdf("text-en.pdf", [
    ["Quarterly report", "The marmalade project shipped on time.", "Budget used: 4200 euros."],
    ["Second page", "The zeppelin inspection is scheduled for Tuesday."],
], "Latin")
text_pdf("text-th.pdf", [
    ["รายงานประจำไตรมาส", "โครงการมะม่วงเสร็จตามกำหนด", "งบประมาณที่ใช้ 4200 บาท"],
], "Thai")
text_pdf("many-pages.pdf", [[f"Page number {i} has the unique word flamingo{i}."] for i in range(1, 41)], "Latin")
text_pdf("encrypted.pdf", [["This text is protected."]], "Latin", password="s3cret")
scanned_pdf("scanned-en.pdf", ["Scanned invoice 2024", "Total due: 1250 dollars"], LATIN)
scanned_pdf("scanned-th.pdf", ["ใบแจ้งหนี้สแกน", "ยอดรวม 1250 บาท"], THAI)
data = open("text-en.pdf", "rb").read()
open("corrupt.pdf", "wb").write(data[:400] + b"\x00\xff garbage " * 20)

# images
def gradient(w, h):
    img = Image.new("RGB", (w, h))
    px = img.load()
    for x in range(w):
        for y in range(h):
            px[x, y] = (255 * x // w, 255 * y // h, 128)
    return img

# 120x80 landscape pixels, EXIF orientation 6 = "rotate 90 CW to display", so it must come out 80x120
img = gradient(120, 80)
d = ImageDraw.Draw(img)
d.rectangle([0, 0, 20, 20], fill=(255, 0, 0))  # red marker in the top-left corner of the stored pixels
exif = Image.Exif()
exif[0x0112] = 6
img.save("photo-exif6.jpg", quality=90, exif=exif)
gradient(3000, 2000).save("big.jpg", quality=80)
rgba = Image.new("RGBA", (64, 64), (0, 0, 0, 0))
ImageDraw.Draw(rgba).ellipse([8, 8, 56, 56], fill=(255, 0, 0, 255))
rgba.save("alpha.png")
frames = [gradient(40, 40), Image.new("RGB", (40, 40), (0, 255, 0))]
frames[0].save("anim.gif", save_all=True, append_images=frames[1:], duration=100, loop=0)
gradient(90, 60).save("pic.webp", quality=80)

# text files
open("thai-tis620.csv", "wb").write("ชื่อ,จำนวน\nแอปเปิ้ล,3\nส้ม,5\n".encode("cp874"))

# a PNG that claims to be absurdly large (header only: enough for DecodeConfig)
def chunk(tag, body):
    c = struct.pack(">I", len(body)) + tag + body
    import zlib
    return c + struct.pack(">I", zlib.crc32(tag + body) & 0xffffffff)
ihdr = struct.pack(">IIBBBBB", 100000, 100000, 8, 2, 0, 0, 0)
open("bomb.png", "wb").write(b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", ihdr) + chunk(b"IEND", b""))
print("ok")
