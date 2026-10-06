---
name: csv-summary
description: Summarise a CSV file (row count, columns, numeric statistics). Use when the user shares or mentions CSV data and wants an overview.
---

# CSV summary

Use this when the user gives you CSV data and wants to know what is in it.

1. Save the CSV text into a file in your workspace, for example with a here-document:
   `cat > data.csv <<'CSV'` … `CSV`.
2. Run the helper script from this skill:
   `python3 /skills/csv-summary/summarize.py data.csv`
   (use the skill folder path shown below if it differs).
3. Explain the result to the user in plain language: how many rows, which columns,
   and anything notable (empty cells, min/max/average of numeric columns).

The script reads the file only, never modifies it, and works with comma, semicolon and tab separated files.
