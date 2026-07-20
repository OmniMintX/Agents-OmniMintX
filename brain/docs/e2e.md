# E2E: chạy 1 goal thật qua AO daemon

Kịch bản chạy tay từng bước + kết quả kỳ vọng, và nhật ký chạy THẬT (kể cả fail/blocked).

## Điều kiện tiên quyết (GATE)

1. AO daemon đang chạy: `cat ~/.ao/running.json` (pid phải còn sống) và
   `curl http://127.0.0.1:3001/api/v1/projects` trả 200.
   - AO là app desktop (`/Applications/Agent Orchestrator.app`); app tự chạy daemon.
     Nếu daemon chết: `open -a "Agent Orchestrator"` rồi poll lại endpoint trên.
   - Lưu ý: `running.json` có thể STALE (pid đã chết nhưng file còn) → luôn kiểm tra
     bằng HTTP, không tin file.
2. `ANTHROPIC_API_KEY` có trong env (planner gọi Anthropic Messages API trực tiếp;
   thiếu key → `om plan` fail với "anthropic: API key is empty").
3. Build om: `cd brain && go build -o /tmp/om ./cmd/om`.

## Kịch bản chính (A→B dependency chaining)

1. Tạo repo test tạm NGOÀI workspace: `/tmp/om-e2e-<ts>`, `git init -b main`,
   commit README. Đăng ký vào AO: `POST /api/v1/projects` (id/name/path).
2. `om plan "goal"` — goal 2 task chuỗi:
   - A: tạo `greeting.txt` với nội dung cụ thể (vd "hello-om-e2e-<ts>").
   - B: đọc `greeting.txt`, tạo `reply.txt` TRÍCH nội dung của A
     (để chứng minh B thật sự thấy code A, không chỉ thấy tên file).
3. `om approve <plan-id>` → `om run <plan-id>`.

Kỳ vọng từng bước:
- Session `om-<hash8>` của A xuất hiện trong AO UI; A chạy → idle + `.om-done`.
- Scheduler merge PR của A TRƯỚC khi dispatch B (merge-before-dispatch).
- B dispatch từ default branch ĐÃ chứa `greeting.txt`; B done → plan done.
- Verify sâu: `reply.txt` chứa nội dung `greeting.txt`; default branch chứa cả 2 file;
  `om status` liệt kê PR/branch từng task; `om events` cho chuỗi
  plan_created→approved→run_started→dispatching/dispatched→started→done×2→plan_done.

### RISK PR-chaining (phải trả lời bằng thực nghiệm)
`prompt.tmpl` KHÔNG bắt agent push/tạo PR. Nếu worker AO không tự mở PR →
`ensureParentsMerged` coi 0 PR là "đã merge" → B KHÔNG thấy code A → ghi FAIL
trung thực + đề xuất fix (prompt bắt push + AO mở PR, hoặc scheduler dùng branch
của cha làm base). KHÔNG làm E2E "xanh giả".

## Kịch bản phụ (resume idempotent)

Kill `om run` giữa lúc A đang chạy → `om run` lại → đếm session trong AO:
không được tạo session trùng (adopt theo displayName marker).

---

## Nhật ký chạy thật — 2026-07-20

**Verdict: BLOCKED** (thiếu ANTHROPIC_API_KEY). E2E chính CHƯA chạy — không fake kết quả.

### Pre-fix (bắt buộc trước E2E) — DONE
1. **displayName collision — FIXED**: planner sinh task id t1..tN theo plan →
   `"om-"+taskID[:8]` trùng giữa các plan cùng project, resume có thể adopt nhầm
   session plan khác. Fix (ít xâm lấn): `displayNameFor(planID, taskID)` =
   `"om-" + hex(sha256(planID+"\x00"+taskID))[:8]` (11 rune < cap 20). Kèm unit test
   `TestDisplayNameFor` (khác plan không trùng, deterministic, đúng độ dài).
   `go test -race ./...` sạch toàn bộ (aoclient/config/planner/scheduler/store).
   Commit 4ffd6c6.
2. **Toàn bộ brain/ đã commit**: 33 file untracked (planner/aoclient/store/config/
   cmd/status) commit 3c3b5ee. `git status` brain/ sạch.

### GATE — kết quả thật
- `~/.ao/running.json` tồn tại (pid 61982, started 13:58Z) nhưng pid ĐÃ CHẾT,
  port 3001 connection refused → run-file stale.
- Khởi động lại: `open -a "Agent Orchestrator"` → daemon UP sau ~2s
  (pid 7846, port 3001). `GET /api/v1/projects` = 200, 1 project `omnimintx`.
- **Version AO (mốc pin)**: app bundle **0.10.3**
  (`CFBundleShortVersionString`; daemon không có endpoint /version hay /openapi —
  các path /api/v1/openapi, /api/v1/version, /api/v1/health đều 404).
- **ANTHROPIC_API_KEY: KHÔNG có** — đã kiểm tra (chỉ check tồn tại, không in giá trị):
  env hiện tại, zsh interactive (`zsh -ic`), rc files (~/.zshrc, ~/.zshenv,
  ~/.zprofile, ~/.bashrc, ~/.profile), `launchctl getenv`, `~/.overmind/config.yaml`
  (không tồn tại), `.env` local. → `om plan` chắc chắn fail ("anthropic: API key is
  empty", planner/anthropic.go) → DỪNG theo GATE.

### Chưa thực hiện (chờ unblock)
- BƯỚC 2-4: repo test tạm, om plan/approve/run, verify chaining, RISK PR-chaining,
  kịch bản resume. **RISK PR-chaining vẫn CHƯA có câu trả lời thực nghiệm.**

### Cách unblock
Đặt key rồi chạy lại từ BƯỚC 1: `export ANTHROPIC_API_KEY=...` (hoặc ghi
`~/.overmind/config.yaml` với `llm.api_key_env` trỏ tới env var khác đã có).

### Phát hiện ngoài lề (chưa fix — ngoài scope pre-fix)
- Run-file stale: AO app không dọn `running.json` khi daemon chết. Overmind đã xử lý
  đúng hướng (connection refused = ErrDaemonNotRunning → backoff, không fail plan),
  nhưng doc/e2e phải luôn verify bằng HTTP thay vì tin run-file.
