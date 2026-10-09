# การ deploy และดูแล jannyq

คู่มือนี้สำหรับคนที่เปิดบอทให้ผู้อื่นใช้: อะไรเปิดออกภายนอก ทำอย่างไรให้ปลอดภัย ดูสถานะ สำรองข้อมูล และอัปเกรด
เนื้อหาอิงชุด Docker Compose ในรีโพนี้ (ท้ายเอกสารมีตัวอย่าง systemd) และมีรายละเอียดเต็มกว่าใน [DEPLOYMENT.md](DEPLOYMENT.md)

> **อะไรที่ตรวจแล้วและยังไม่ได้ตรวจ** โค้ด Go ในคู่มือนี้ (metrics, backup, restore, retention, readiness) มี test และลองรันเป็นไบนารีจริงแล้ว
> แต่ **Docker image, ไฟล์ docker-compose และ systemd unit ยังไม่เคย build หรือรันจริง** (เครื่องที่เขียนไม่มี Docker daemon และ systemd)
> ให้เริ่มจากระบบทดสอบ แล้วรัน `docker compose config`, `docker compose build` และซ้อมกู้ข้อมูลก่อนใช้งานจริง

## 1. ส่วนประกอบและขอบเขตความเชื่อใจ

| ส่วน | เก็บอะไร | ติดต่อใคร |
|---|---|---|
| **jannyq** | token ของช่องทาง, API key, บทสนทนาทั้งหมด, กุญแจ session ของเว็บ | โมเดล, SearXNG, อินเทอร์เน็ต (web_fetch), sandbox |
| **sandbox** | ไม่มี secret ของบอท, workspace ของแต่ละแชท | ไม่ออกนอก network ภายใน เว้นแต่เปิด egress |
| **Caddy** | ใบรับรอง TLS | jannyq เท่านั้น |

หลักการ: **อะไรที่โมเดลหรือผู้ใช้ควบคุมได้ ให้ทำใน sandbox ซึ่งไม่มี secret** บอทเองไม่รันคำสั่ง และไฟล์ PDF ก็ถูกอ่านใน sandbox

## 2. เช็กลิสต์ก่อนเปิดสาธารณะ

- [ ] **ใครคุยได้บ้าง** `JANNYQ_ALLOWED_USERS` และ `JANNYQ_ALLOWED_GROUPS` (ว่างทั้งคู่ = ทุกคน), `JANNYQ_RATE_LIMIT`, และ `JANNYQ_WEB_ACCESS_CODE` สำหรับ web chat — เวลาของโมเดลคือทรัพยากรที่แพง
- [ ] **คำสั่ง** ใช้ `JANNYQ_RUN_COMMAND=sandbox` เท่านั้น อย่าใช้ `host` นอกจากทดลองส่วนตัว และคง `JANNYQ_SANDBOX_NETWORK=off`
- [ ] **ความลับ** สร้าง `JANNYQ_SANDBOX_TOKEN` ด้วย `openssl rand -hex 32` ใช้ไฟล์ (`..._FILE`) แทน environment variable เมื่อทำได้ และห้าม commit `.env`
- [ ] **HTTPS** ช่องทาง webhook (LINE, Messenger, WhatsApp) และ web chat ผ่าน `docker-compose.public.yml` เท่านั้น มีแต่ Caddy ที่เปิดพอร์ต
- [ ] **แยก sandbox ให้แข็งแรงขึ้น** ถ้าคนแปลกหน้ารันคำสั่งได้: ติดตั้ง [gVisor](https://gvisor.dev) แล้วเปิด `runtime: runsc` ใน `docker-compose.yml` หรือแยกไปอีกเครื่อง
  (container ใช้ kernel ร่วมกับเครื่องหลัก)
- [ ] **โควตาดิสก์** ตั้งขนาดสูงสุดให้ volume ของข้อมูลและ `/work` เพราะข้อมูลโตตามจำนวนแชท
- [ ] **ความเป็นส่วนตัว** กำหนดระยะเก็บบทสนทนา (`JANNYQ_RETENTION_DAYS`) และบอกผู้ใช้ว่าเก็บอะไร (หัวข้อ 7)
- [ ] **อัปเดต** build image ใหม่สม่ำเสมอ (`docker compose build --pull`) CI รัน `govulncheck` ตรวจช่องโหว่ของ dependency และไลบรารีมาตรฐานของ Go

ที่ตั้งใจไม่ทำ: sandbox แบบหนึ่ง container ต่อหนึ่งแชทผ่าน Docker socket เพราะต้องให้บอทถือ socket ของ Docker ซึ่งเท่ากับให้ผู้ที่เจาะบอทได้คุมทั้งเครื่อง
ตรงข้ามกับเป้าหมายของ sandbox การแยก user ต่อแชท ร่วมกับ gVisor (หรืออีกเครื่อง) ให้การแยกที่แข็งแรงโดยไม่ต้องทำเช่นนั้น

## 3. ติดตั้ง

```sh
cp .env.example .env              # ตั้งโมเดล JANNYQ_SANDBOX_TOKEN และช่องทางอย่างน้อยหนึ่งช่องทาง
docker compose config >/dev/null  # จับพิมพ์ผิดก่อนเริ่ม
docker compose up -d --build
# เปิดสาธารณะ: ตั้ง JANNYQ_DOMAIN แล้ว
docker compose -f docker-compose.yml -f docker-compose.public.yml up -d --build
```

ลำดับตอนเริ่ม: ระบบตรวจและสร้างฐานความรู้ให้เสร็จ **ก่อน** เปิดเว็บเซิร์ฟเวอร์และ channel (`JANNYQ_KNOWLEDGE_STARTUP_SYNC`) ดังนั้น `/healthz` จะยังไม่ตอบจนกว่าจะเสร็จ
image ให้เวลา 2 นาที (`--start-period`) ถ้าโฟลเดอร์ใหญ่ให้เพิ่ม `start_period` ใน `healthcheck:` ของ compose (หรือตั้ง `JANNYQ_KNOWLEDGE_STARTUP_SYNC=false`) เพื่อไม่ให้คอนเทนเนอร์ถูกมองว่า unhealthy ระหว่างรอ

ตรวจสถานะ: `/healthz` (โพรเซสทำงานอยู่) และ `/readyz` (เขียนโฟลเดอร์ข้อมูลได้และฐานความรู้ตอบ — ไม่ขึ้นกับโมเดลหรือแพลตฟอร์มอื่น)

## 4. Monitoring

ตั้ง `JANNYQ_METRICS_LISTEN=:9100` กับ `JANNYQ_METRICS_TOKEN` metrics อยู่บน **พอร์ตของตัวเอง** ไม่ผ่านเว็บเซิร์ฟเวอร์สาธารณะ และ compose ไม่เปิดพอร์ตนี้ออกนอก
ให้ Prometheus ใน network เดียวกันมา scrape รายชื่อ metric ตัวอย่าง `prometheus.yml` และกฎแจ้งเตือนอยู่ใน [DEPLOYMENT.md](DEPLOYMENT.md#4-monitoring)
สิ่งที่ควรแจ้งเตือน: ไม่มี backup เกิน 2 วัน (`jannyq_last_backup_timestamp_seconds`), backup ล้มเหลว, โมเดล error สูง, ตอบช้า, ฐานความรู้สแกนไม่คืบ, บอทล่ม

## 5. Backup และกู้ข้อมูล

ตั้ง `JANNYQ_BACKUP_DIR` เพื่อสำรองอัตโนมัติทุก `JANNYQ_BACKUP_INTERVAL` (24 ชม.) เก็บล่าสุด `JANNYQ_BACKUP_KEEP` (7) ชุด
แต่ละชุดคือ `jannyq-backup-<เวลา UTC>.tar.gz` (สิทธิ์ 0600) มีฐานข้อมูลของทุกแชท ฐานความรู้ ไฟล์ที่ผู้ใช้ส่ง (ปิดได้ด้วย `JANNYQ_BACKUP_FILES=false`)
กุญแจ session ของเว็บ และ audit log พร้อม manifest ที่มี checksum ของทุกไฟล์ ฐานข้อมูลถูกคัดลอกแบบ snapshot ที่สอดคล้องกันขณะบอททำงาน (`VACUUM INTO`)
ไฟล์ถูกเขียนชื่อชั่วคราวแล้วเปลี่ยนชื่อเมื่อเสร็จ **ไฟล์นี้มีบทสนทนาส่วนตัวและกุญแจ จึงต้องปกป้องเหมือนข้อมูลจริง**

**ดิสก์เดียวกันไม่ใช่ backup** ให้คัดลอกออกนอกเครื่อง (เช่น `rclone sync` จาก cron) และเข้ารหัสที่ปลายทาง

```sh
docker compose exec jannyq jannyq backup --out /backups     # สำรองทันที
docker compose exec jannyq jannyq verify /backups/jannyq-backup-….tar.gz
docker compose stop jannyq
docker compose run --rm --no-deps jannyq restore --from /backups/….tar.gz            # โฟลเดอร์ข้อมูลว่าง
docker compose run --rm --no-deps jannyq restore --from /backups/….tar.gz --force    # แทนที่ข้อมูลเดิม (ย้ายไว้ข้าง ๆ ไม่ลบ)
docker compose start jannyq
```

ต้องหยุดบอทก่อนกู้ ระบบตรวจไฟล์ทั้งหมดก่อนแตะข้อมูลของคุณ (path, ชนิดไฟล์, ขนาด, checksum, ความสมบูรณ์ของฐานข้อมูล) ไฟล์ที่เสียหรือประสงค์ร้ายถูกปฏิเสธและไม่ทิ้งอะไรไว้
workspace ของ sandbox ไม่อยู่ใน backup กู้เฉพาะ backup ที่คุณทำเอง เพราะฐานข้อมูลจากแหล่งที่ไม่น่าไว้ใจอาจมี trigger หรือ view ที่ทำงานเมื่อบอทใช้งาน
**ควรซ้อมกู้ข้อมูลบนเครื่องสำรองหนึ่งครั้งก่อนต้องใช้จริง**

## 6. อัปเกรด

1. สำรองข้อมูล (`jannyq backup`) แล้วคัดลอกออกนอกเครื่อง
2. `git pull && docker compose build --pull && docker compose up -d`
3. ดู log และ `/readyz`

schema ของฐานข้อมูลอัปเกรดไปข้างหน้าอัตโนมัติ **ไม่มีการย้อนกลับ** ถ้าต้องถอยเวอร์ชัน ให้กู้ backup ที่ทำไว้พร้อม image เก่า

## 7. เก็บอะไรไว้ และนานแค่ไหน

| ข้อมูล | อยู่ที่ | ลบด้วย |
|---|---|---|
| ข้อความ สรุป ใครพูดอะไรเมื่อไร | `<data>/sessions/<channel>/<chat>/session.db` | `/reset`, `JANNYQ_RETENTION_DAYS`, ลบโฟลเดอร์ |
| ไฟล์ที่ผู้ใช้ส่ง | `…/<chat>/files/` | เช่นเดียวกัน (เก่าสุดก่อนเมื่อเกิน `--attach-chat-mb`) |
| สำเนาให้ `run_command` | workspace ของ sandbox `inbox/` | `/reset`, ล้าง workspace ที่ไม่ใช้ 7 วัน |
| เตือน/งานตามเวลา | `<data>/triggers.db` (ไม่บันทึกข้อความลง log) | เจ้าของลบในแชท, `JANNYQ_RETENTION_DAYS` (พร้อมแชท), ลบไฟล์ |
| ฐานความรู้ | `<data>/knowledge.db` | โฟลเดอร์ต้นทางคือความจริง ลบไฟล์แล้วดัชนีตาม |
| กุญแจ session ของเว็บ | `<data>/web_secret` | เปลี่ยนแล้วทุกคนต้องเข้าใหม่ |
| log คำสั่ง | `<data>/audit/commands.jsonl` | หมุนที่ 10 MB |
| Backup | `JANNYQ_BACKUP_DIR` | `JANNYQ_BACKUP_KEEP` และสำเนานอกเครื่องของคุณ |

`JANNYQ_RETENTION_DAYS=N` ลบแชทที่ไม่ถูกเขียนมา N วัน (ทั้งข้อความและไฟล์) ทุกชั่วโมง แชทที่กำลังถูกใช้งานไม่ถูกแตะ ผู้ใช้ที่กลับมาเริ่มใหม่
backup ยังเก็บของที่ลบไปแล้วจนกว่าจะหมุนออก ลบข้อมูลคนเดียว: ลบโฟลเดอร์แชทของเขา (`sessions/<channel>/<id>-<hash>/`) และ workspace ใน sandbox (`/reset` ทำให้)

## 8. ไม่ใช้ Docker (systemd)

ใน `deploy/systemd/` มี `jannyq.service` (บอท รันเป็น user ธรรมดาพร้อมข้อจำกัดของ systemd) และ `jannyq-sandbox.service`
(executor ต้องเริ่มเป็น root เพื่อแยก user ต่อแชท แต่เหลือ capability เท่าที่จำเป็น) ตรวจผลด้วย `systemd-analyze security jannyq.service`
ไฟล์เหล่านี้เป็นตัวอย่างที่ยังไม่ได้รันจริง
