# Overmind — Tổng kết tiến độ

Tài liệu kỹ thuật tổng hợp những gì đã làm, kết quả thật (kể cả fail), và kế hoạch đang chạy. Nhật ký E2E chi tiết: [brain/docs/e2e.md](../brain/docs/e2e.md).

## Tổng quan

Overmind là **coordinator chạy trên AO daemon** (agent-orchestrator): gõ 1 goal → Overmind sinh plan dạng DAG, user duyệt, rồi tự động spawn/giám sát các session worker trên AO theo thứ tự dependency — không cần người ngồi canh từng wave.

Quyết định nền (đã chốt):
- **Compose, không fork AO**: Overmind là process Go riêng, gọi HTTP API loopback của AO daemon. Không sửa code AO.
- **Event sourcing trên SQLite**: mọi quyết định của bộ não là event append-only trong `brain_events`; state derive từ event log → crash-resume/replay được.
- **Go**, module riêng `brain/go.mod`, CLI tên `om`.
- Overmind KHÔNG tự viết code — chỉ plan/dispatch/giám sát. LLM chỉ gọi ở điểm rẻ nhất (plan 1 lần/goal).

## Phase 1 — MVP Bộ não (ĐÓNG 2026-07-20)

### Các mảnh đã build (tất cả trong `brain/`)
- `internal/store` — SQLite: plans/tasks/dependencies + `brain_events` append-only; run-lock có heartbeat.
- `internal/planner` — goal → JSON plan (DAG), **đa provider**: `anthropic` (Messages API), `openai` (OpenAI-compatible, base URL cấu hình được → phủ DeepSeek/Ollama/LM Studio), `cli` (gọi harness CLI headless như `claude -p` — dùng subscription, không cần API key).
- `internal/aoclient` — HTTP client cho AO daemon (sessions, preview files, project…).
- `internal/scheduler` — ready queue theo dependency, dispatch/adopt session, poll trạng thái, verdict marker, merge, timeout.
- `internal/gitops` — local merge (ancestor-check idempotent, conflict → fail deterministic, recovery MERGE_HEAD).
- `cmd/om` — CLI: `om plan / approve / run / status / events`.

### Kiến trúc chaining + done-marker (thiết kế chốt sau 3 red-team độc lập)
- **Chaining = local merge-before-finish**: AO 0.10.3 không mở PR (endpoint MergePR của AO là stub TODO) → Overmind tự merge local nhánh session cha vào default branch ngay khi cha done-ok, TRƯỚC FinishTask. Con dispatch từ main đã chứa code cha.
- **Done-marker per-task có cấu trúc**: file `.om-done.<hex8>` (hex8 = hash(planID, taskID) → miễn nhiễm marker cha lọt vào base branch). Dòng đầu `ok: ...` | `fail: ...`; footer protocol do scheduler tiêm lúc dispatch (không ủy quyền planner-LLM). Verdict 5 ngả: ok→merge+done; fail→failed không merge; rỗng→chờ; malformed→grace; thiếu→timeout 10 phút.
- **Stale run-lock heartbeat**: `om run` bị kill -9 để lại lock trong SQLite; run mới phát hiện holder chết (heartbeat-based) và steal lock an toàn — resume không bị chặn oan.

### Kết quả E2E — 3 lần chạy, ghi trung thực
1. **Lần 1 (2026-07-20): BLOCKED** — thiếu `ANTHROPIC_API_KEY`, GATE dừng đúng thiết kế, không fake kết quả. Unblock bằng cách thêm planner `provider=cli`.
2. **Lần 2 (2026-07-20→21): FAIL trung thực** — pipeline chạy hết A→B và plan "done", nhưng chaining VỠ đúng như RISK dự đoán: worker AO không mở PR, `ensureParentsMerged` coi 0 PR = merged → task con thiếu code cha. Đồng thời phát hiện tiêu chí "done" quá dễ dãi (chỉ idle + marker tồn tại → marker nội dung failure vẫn thành done). Sinh task OM-6b sau 3 nghiên cứu red-team song song. Xác nhận thêm: **MergePR của AO là stub** — merge "thành công" mà không merge gì.
3. **Lần 3 (2026-07-21, build 65242f4): PASS cả 3 kịch bản**, đối chiếu hiện trường vật lý (không chỉ tin báo cáo agent):
   - **Chaining A→B**: main chứa file của cả 2 task, `task_branch_merged` đúng thứ tự, t2 chạy không cần bấm gì.
   - **Fail thật → không merge**: agent ghi `fail:` → `task_failed kind=marker_fail`, không có merge event, main không đổi.
   - **Kill-resume**: kill -9 → run mới steal stale lock, adopt session cũ, không dispatch trùng, plan_done.

   **Verifier độc lập audit: PASS, confidence High** — go vet + go test -race sạch toàn module (126 test / 7 package), gitops có 10 test trên repo git thật, E2E khớp 100% nhật ký.

### Rủi ro tồn dư đã ghi nhận (non-blocker) + trạng thái vá
| # | Rủi ro | Trạng thái |
|---|--------|-----------|
| 1 | `merge --abort` vô điều kiện có cửa sổ TOCTOU; 2 plan song song cùng repo có thể fail cứng vì index.lock | **Đã vá — OM-13, commit 8cfddb1** (guard abort bằng MERGE_HEAD==tip + lock per-repo) |
| 2 | MaxBackoff (60s) == LockStaleAfter (60s) → outage dài có thể bị steal lock oan | **Đã vá — OM-13, commit 8cfddb1** (heartbeat trong backoff) |
| 3 | taskClock in-memory reset khi resume | Chấp nhận Phase 1, đã doc |
| 4 | Trust-folder dialog của `.claude/settings.json` cần 1 lần bấm tay ở session đầu | Chuyển Phase 2 (OM-11: AO project config permissions) |

### Ranh giới Phase 1
Chỉ hỗ trợ **repo remoteless** (có `origin/<default>` → fail-fast ngay từ precheck). Repo có remote + PR path để Phase 3+.

## Phase 2 — Verifier Loop + Autonomy + Approval Gates (đang chạy)

### Thứ tự ưu tiên (chốt với user 07/2026)
1. **Verifier loops + retry-with-feedback** — lỗ hổng lớn nhất: "done" hiện là tự khai của worker.
2. **Autonomy qua AO project permissions** — xóa trust-dialog + allowlist tĩnh `.claude/settings.json`.
3. **Approval gates tầng Overmind** — nâng cấp trên nền needs_human đã hoạt động.

### Thiết kế lõi
- **Verify 3 tầng**: tầng 0 deterministic (diff không rỗng + lệnh check planner sinh per-task, miễn phí, bắt buộc); tầng 1 verifier LLM qua provider `cli` (verdict ghi `.om-verdict.<hex8>`); tầng 2 API provider (tùy chọn qua roles). Planner gán `verify: none | deterministic | llm` per task.
- **Merge pipeline kiểu Intent**: `marker ok:` → verify tầng 0 → tầng 1 (nếu có) → **system-commit** (scheduler tự commit thay đổi worker bỏ sót) → local merge. Merge là bước cuối của chuỗi kiểm.
- **Retry with feedback**: FAIL → re-dispatch worker kèm feedback verdict; `max_verify_rounds` mặc định 2; hết budget → needs_human (nhánh khác vẫn chạy).
- **Named providers + roles**: config v2 `providers.<tên>` + `roles.planner|verifier` → { provider, model }; tương thích ngược config cũ.
- **Autonomy**: `SetWorkerPermissions` kiểu GET-merge-PUT trên AO project config (SetConfig của AO thay thế toàn bộ config — đã có bằng chứng source).
- **Approval gates**: `requires_approval` chặn TRƯỚC dispatch, `om approve-task / reject-task`, notification macOS qua osascript.

### Tasks + Waves
7 task OM-8 → OM-14, chạy theo 4 wave:
- **Wave 1** (song song): OM-8 (config v2), OM-13 (vá rủi ro audit — độc lập)
- **Wave 2**: OM-9 (verify tầng 0 + merge pipeline + system-commit) → OM-10 (verifier LLM + retry budget; cần OM-8)
- **Wave 3** (song song): OM-11 (autonomy), OM-12 (approval gates + notification)
- **Wave 4**: OM-14 (E2E Phase 2, 5 kịch bản) → verifier độc lập → đóng Phase 2

### Trạng thái hiện tại (2026-07-21)
- **Wave 1**: OM-13 **xong** (commit 8cfddb1), OM-8 **đang chạy**.
- Các wave sau chưa bắt đầu.

## Nghiên cứu bên ngoài — herdr (đánh giá 2026-07-21)

[herdr](https://github.com/ogulcancelik/herdr) (~18.9k sao, v0.7.4 07/2026, Rust 1 binary, AGPL-3.0 + dual-license thương mại) — **agent multiplexer trong terminal**, kiểu tmux chuyên cho AI coding agent: bảng trạng thái blocked/working/done trên terminal view thật, detach/reattach qua SSH, session sống sót restart, socket API cho agent tự spawn pane/đọc output/chờ nhau, plugin marketplace.

**Kết luận: KHÔNG đưa vào core.** herdr không cạnh tranh với Overmind (không planner, không DAG, không merge, không verify) — nó cạnh tranh một phần với AO daemon, nhưng thiếu những thứ Overmind phụ thuộc: worktree + branch `ao/<session>/root` per-session, PreviewFile (dò marker), displayName (idempotency key), 13 derived status. Thay AO bằng herdr = tự xây lại toàn bộ phần đó. "Compose, không fork AO" vẫn đúng hướng.

Hai chỗ dùng được:
1. **Tầng quan sát/can thiệp `needs_human` (ngắn hạn)**: chạy worker session trong herdr thay tmux trần → thấy ngay agent nào blocked, attach từ xa để bấm trust-dialog/approval (pain point E2E lần 3). Dùng như tool ngoài → AGPL không ảnh hưởng. Caveat: cần kiểm chứng AO có cho cấu hình nơi chạy session; và OM-11 (PUT project config permissions) giải gốc rễ tốt hơn — herdr chỉ làm việc bấm tay dễ chịu hơn.
2. **Ứng viên runtime thay AO (Phase 3+, dự phòng)**: socket API spawn/read/wait + session persistence là nền khả dĩ nếu muốn thoát AO 0.10.3 (MergePR stub, không API PR, thiếu route workspace/files). Chi phí: viết lại aoclient + tự quản worktree/branch — không đáng khi Phase 2 chưa xong.
