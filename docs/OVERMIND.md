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

## Repo tham khảo cần bổ sung (khảo sát 2026-07-21)

Bộ `docs/repo/` hiện tại (26 repo, gitignore — KHÔNG theo repo khi chuyển máy, clone lại theo danh sách này) thiên về multiplexer/kanban UI, thiếu 5 mảng. 14 ứng viên dưới đây đã xác minh sống bằng fetch trang GitHub (loại repo chết/archive), xếp hạng theo giá trị cho Overmind.

### Đợt 1 — clone ngay, phục vụ Phase 2

| Repo | Sao | Gap | Vì sao |
|---|---|---|---|
| [anthropic-experimental/sandbox-runtime](https://github.com/anthropic-experimental/sandbox-runtime) | ~4.7k | OM-11 | Sandbox chính chủ Anthropic cho Claude Code: Seatbelt (macOS) + bubblewrap/seccomp (Linux), proxy allowlist domain — cách chuẩn chạy `--dangerously-skip-permissions` an toàn, gọi được từ Go. Lưu ý org là `anthropic-experimental`, không phải `anthropics`. |
| [qodo-ai/pr-agent](https://github.com/qodo-ai/pr-agent) | ~12.2k | OM-10 | Blueprint verifier LLM: nén diff + structured review; output `/improve` = payload feedback cho retry-with-feedback. |
| [humanlayer/humanlayer](https://github.com/humanlayer/humanlayer) | ~11.1k | OM-12 | Go daemon (`hld`) quản Claude Code session + route tool-call approval cho người. Caveat: một phần deprecated chờ rebuild. |
| [cschleiden/go-workflows](https://github.com/cschleiden/go-workflows) | ~511 | durability | Durable workflow engine nhúng, Go, backend SQLite, event-sourced history + deterministic replay — gần nhất với đúng thiết kế store của Overmind; tham chiếu số 1 cho schema/replay. |
| [confident-ai/deepeval](https://github.com/confident-ai/deepeval) | ~17k | OM-10 | Primitives LLM-as-judge (G-Eval, rubric + threshold pass/fail) cho quyết định accept-or-retry của verifier. |

### Đợt 2 — khi vào Phase 3 (merge queue / re-planning / budget)

| Repo | Sao | Gap | Vì sao |
|---|---|---|---|
| [rust-lang/bors](https://github.com/rust-lang/bors) | ~165 | merge-queue | Merge queue production của rust-lang: serialize merge, test kết quả merge trước khi tiến main, rollup batching — lời giải cho diamond DAG. |
| [SWE-bench/SWE-bench](https://github.com/SWE-bench/SWE-bench) | ~5.5k | OM-10 | Chuẩn mực test-based gating: apply diff trong container per-instance, chạy test suite thật → pass/fail. |
| [dbos-inc/dbos-transact-golang](https://github.com/dbos-inc/dbos-transact-golang) | ~765 | durability | Step-checkpointing (resume từ step cuối) — mô hình crash-resume đơn giản hơn full replay để đối chiếu. |
| [sentient-agi/ROMA](https://github.com/sentient-agi/ROMA) | ~5.1k | replanning | Atomizer/Planner/Aggregator/Verifier đệ quy — mẫu dynamic decomposition + re-plan giữa chừng. |
| [riverqueue/river](https://github.com/riverqueue/river) | ~5.5k | durability | Pattern Go chuẩn enqueue-trong-transaction — khớp kỷ luật "1 transaction = event + cache" của store. |
| [jj-vcs/jj](https://github.com/jj-vcs/jj) | ~30.5k | integration | Conflict là object first-class, auto-rebase descendants, operation log — tích hợp song song chịu conflict khi nhiều branch agent đổ về main. |
| [maximhq/bifrost](https://github.com/maximhq/bifrost) | ~6.7k | budget | AI gateway Go với module governance: virtual keys, budget phân cấp, rate limit — tham chiếu budget/token per-task. |
| [microsandbox/microsandbox](https://github.com/microsandbox/microsandbox) | ~7k | OM-11 | MicroVM self-hosted cho workload không tin cậy, có Go SDK — cô lập mạnh hơn container cho session không giám sát. |
| [ServiceNow/TapeAgents](https://github.com/ServiceNow/TapeAgents) | ~318 | replanning | Toàn bộ session là "Tape" replayable, revise plan giữa chừng, resume từ bất kỳ điểm nào — cùng triết lý event-sourcing. |

### Honorable mentions (đã xác minh, dưới ngưỡng)
restatedev/restate (durable execution single-binary, Rust), RchGrav/claudebox (Docker harness cho Claude Code — OM-11), maragudk/goqite (SQLite queue tối giản), chdsbd/kodiak (auto-merge bot gọn — Phase 3 PR-flow), kubernetes-sigs/agent-sandbox + Zouuup/landrun (sandbox 2 đầu phổ nặng/nhẹ — OM-11), kodus-ai (review rules ngôn ngữ tự nhiên → gợi ý acceptance criteria per-task).

Lưu ý: **mergiraf** (merge cấu trúc syntax-aware) khớp gap integration nhưng ở Codeberg, chưa xác minh — xem tay. Các repo đã loại vì chết/archive: textcortex/claude-code-sandbox, lmnr-ai/flow, marge-bot, bors-ng, Agentless, mergify-engine (đóng nguồn).

### Lệnh clone (chạy trên máy mới, trong `docs/repo/`)
```sh
cd docs/repo
# Đợt 1
for r in anthropic-experimental/sandbox-runtime qodo-ai/pr-agent humanlayer/humanlayer cschleiden/go-workflows confident-ai/deepeval; do git clone --depth 1 "https://github.com/$r.git"; done
# Đợt 2 (khi vào Phase 3)
for r in rust-lang/bors SWE-bench/SWE-bench dbos-inc/dbos-transact-golang sentient-agi/ROMA riverqueue/river jj-vcs/jj maximhq/bifrost microsandbox/microsandbox ServiceNow/TapeAgents; do git clone --depth 1 "https://github.com/$r.git"; done
```
