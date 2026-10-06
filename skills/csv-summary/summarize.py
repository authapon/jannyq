#!/usr/bin/env python3
"""Print a short summary of a CSV file: rows, columns and numeric statistics."""
import csv
import statistics
import sys


def to_number(value):
    try:
        return float(value.replace(",", "")) if value.strip() else None
    except ValueError:
        return None


def main(path):
    with open(path, newline="", encoding="utf-8-sig") as f:
        sample = f.read(4096)
        f.seek(0)
        try:
            dialect = csv.Sniffer().sniff(sample, delimiters=",;\t|")
        except csv.Error:
            dialect = csv.excel
        rows = list(csv.reader(f, dialect))
    if not rows:
        print("The file is empty.")
        return 0
    header, data = rows[0], rows[1:]
    print(f"Rows (excluding header): {len(data)}")
    print(f"Columns ({len(header)}): {', '.join(header)}")
    for i, name in enumerate(header):
        cells = [r[i] if i < len(r) else "" for r in data]
        empty = sum(1 for c in cells if not c.strip())
        numbers = [n for n in map(to_number, cells) if n is not None]
        line = f"- {name}: {len(cells) - empty} filled, {empty} empty"
        if numbers and len(numbers) >= (len(cells) - empty) * 0.8:
            line += (f"; numeric min={min(numbers):g} max={max(numbers):g} "
                     f"mean={statistics.fmean(numbers):g}")
        else:
            distinct = len({c for c in cells if c.strip()})
            line += f"; {distinct} distinct values"
        print(line)
    return 0


if __name__ == "__main__":
    if len(sys.argv) != 2:
        print("usage: summarize.py FILE.csv", file=sys.stderr)
        sys.exit(2)
    sys.exit(main(sys.argv[1]))
