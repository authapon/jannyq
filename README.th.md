# jannyq

แชทบอทที่เขียนด้วยภาษา Go เชื่อมต่อแพลตฟอร์มแชทกับโมเดลของ **Ollama** หรือ API ที่เข้ากันได้กับ **OpenAI**
พร้อม tool calling สำหรับค้นเว็บและอ่านหน้าเว็บ

> **สถานะ: Phase 1 (แกนหลัก)** — ช่องทาง Telegram และ terminal, tool `web_search` และ `web_fetch`,
> ความจำแยกตามแชทพร้อมสรุปอัตโนมัติ และ Docker ดู [แผนงาน](#แผนงาน) สำหรับสิ่งที่จะตามมา

## ความสามารถใน Phase 1

- **โมเดล**: Ollama native API (ส่ง `num_ctx` ได้) หรือ endpoint `/chat/completions` ที่เข้ากันได้กับ OpenAI
- **Tools**
  - `web_search` — ค้นผ่าน [SearXNG](https://github.com/searxng/searxng) ที่คุณตั้งไว้
  - `web_fetch` — โหลดหน้าเว็บแล้วแปลงเป็นข้อความอ่านง่าย รองรับ charset เก่าของไทย (เช่น windows-874)
    และอ่านหน้ายาว ๆ ต่อได้ด้วย `offset` กัน SSRF โดยไม่ยอมเชื่อมต่อ IP ภายใน/loopback/link-local
    แม้เป็นปลายทางของ redirect
- **ความจำแยกตามแชท**: ผู้ใช้แต่ละคนและกลุ่มแต่ละกลุ่มมีไฟล์ SQLite ของตัวเอง
  (`<data-dir>/sessions/<channel>/<chat>/session.db`) ลบโฟลเดอร์ของแชทนั้นก็เท่ากับลืมบทสนทนา
- **Compact context**: เมื่อบทสนทนาเกิน `--compact-after` ข้อความ (ค่าเริ่มต้น 200) หรือ prompt ใหญ่เกิน
  `--compact-ratio` ของ `--context-size` ระบบจะให้โมเดลสรุปข้อความเก่าแล้วแทนที่ด้วยสรุป
  ส่วนข้อความล่าสุดคงไว้ตามเดิม
- **หลายภาษา**: ข้อความของระบบมีไทย/อังกฤษ (`--lang`) และสั่งให้โมเดลตอบตามภาษาหลัก
  หรือตามภาษาที่ผู้ใช้พิมพ์ได้ (`--lang-mode follow-user`)
- **ความปลอดภัยพื้นฐาน**: allowlist ผู้ใช้, rate limit ต่อผู้ใช้, คิวต่อแชท, จำกัดจำนวนงานพร้อมกัน,
  จำกัดรอบ tool call และขนาดผลลัพธ์, ไม่แสดง Telegram token ใน log

## เริ่มต้นใช้งาน

### ทดสอบใน terminal (ไม่ใช้ Docker)

```sh
# ต้องมี Ollama ที่รันอยู่และ model ที่รองรับ tool:  ollama pull qwen3:8b
go run ./cmd/jannyq --cli --llm-model qwen3:8b --context-size 16384 --lang th
```

เพิ่ม `--searxng-url http://localhost:8080` เพื่อเปิด `web_search`

### Docker Compose (Telegram + SearXNG + Ollama)

```sh
cp .env.example .env     # ใส่ JANNYQ_TELEGRAM_TOKEN (จาก @BotFather) และเลือกโมเดล
docker compose up -d --build
docker compose logs -f jannyq
```

compose จะเริ่ม jannyq, SearXNG (เปิดรูปแบบ JSON ไว้แล้วใน `deploy/searxng/settings.yml`), Ollama
และตัวช่วยดาวน์โหลดโมเดล ไม่มีพอร์ตใดถูกเปิดออกภายนอก เพราะ Telegram ใช้ long polling ขาออก
ถ้าใช้ API แบบ OpenAI ภายนอก ให้ตั้งค่าใน `.env` แล้วลบ service ของ Ollama ออก

## การตั้งค่า

ทุกค่าเป็นได้ทั้ง flag และ environment variable: `--llm-model` ⇄ `JANNYQ_LLM_MODEL` โดย flag มีลำดับสูงกว่า
ค่าลับอ่านจากไฟล์ได้ด้วยต่อท้าย `_FILE` เช่น `JANNYQ_TELEGRAM_TOKEN_FILE=/run/secrets/tg`
ใช้ `jannyq --help` ดูรายการทั้งหมด (ตารางค่าหลักอยู่ใน [README.md](README.md#configuration))

### ข้อควรรู้เกี่ยวกับ Telegram

- ในกลุ่ม บอทจะตอบเมื่อถูก **@mention**, มีคน **reply ข้อความของบอท** หรือเมื่อมี `/command`
- คำสั่ง: `/help`, `/reset` (ลืมบทสนทนา), `/compact` (สรุปข้อความเก่าทันที)
- ตอนนี้ตอบเป็นข้อความธรรมดา (ยังไม่แสดงผล Markdown)

### การเลือกโมเดล

โมเดลต้องรองรับ **tool calling** ถึงจะใช้ `web_search`/`web_fetch` ได้ (Ollama: qwen3, llama3.1 ขึ้นไป ฯลฯ)
ถ้าโมเดลปฏิเสธ tools ระบบจะบันทึกคำเตือนและคุยต่อโดยไม่ใช้ tools
และควรตั้ง `--context-size` เสมอเมื่อใช้ Ollama เพราะค่าเริ่มต้นของ Ollama เล็กมาก

## แผนงาน

1. ✅ **แกนหลัก** — agent loop, Ollama/OpenAI, ความจำ SQLite ต่อแชท + compaction, `web_search`, `web_fetch`,
   Telegram, CLI, Docker
2. `run_command` ใน sandbox แยก (เปิดให้ทุกคน พร้อม limit/quota/audit log) และตัวโหลด skills
3. Webbot (หน้าเว็บ) และ webhook server กลาง
4. รูปภาพและ PDF (vision ใช้โมเดลหลัก, สกัดข้อความ PDF พร้อม OCR สำรอง)
5. ฐานความรู้ RAG จากโฟลเดอร์ไฟล์ text/PDF — SQLite, ค้นแบบ vector + full-text, sync เมื่อไฟล์แก้ไข/ลบ
6. Discord และ LINE
7. Messenger และ WhatsApp
8. Hardening และ operations
