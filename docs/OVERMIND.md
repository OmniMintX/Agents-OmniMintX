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
| 4 | Trust-folder dialog của `.claude/settings.json` cần 1 lần bấm tay ở session đầu | **Đã vá — OM-11** (autonomy qua AO project config, bỏ settings.json; E2E lần 6 xác nhận dialog biến mất) |

### Ranh giới Phase 1
Chỉ hỗ trợ **repo remoteless** (có `origin/<default>` → fail-fast ngay từ precheck). Repo có remote + PR path để Phase 3+.

## Phase 2 — Verifier Loop + Autonomy + Approval Gates (đang chạy)

### Thứ tự ưu tiên (chốt với user 07/2026)
1. **Verifier loops + retry-with-feedback** — lỗ hổng lớn nhất: "done" hiện là tự khai của worker.
2. **Autonomy qua AO project permissions** — xóa trust-dialog + allowlist tĩnh `.claude/settings.json`.
3. **Approval gates tầng Overmind** — nâng cấp trên nền needs_human đã hoạt động.

### Thiết kế lõi
- **Verify 3 tầng**: tầng 0 deterministic (diff không rỗng + lệnh check planner sinh per-task, miễn phí, bắt buộc); tầng 1 verifier LLM qua provider `cli` (verdict ghi `.om-verdict.<hex8>` — *thiết kế gốc; hiện thực OM-10 dùng event `task_verdict {tier: 1}`, xem divergence bên dưới*); tầng 2 API provider (tùy chọn qua roles). Planner gán `verify: none | deterministic | llm` per task.
- **Merge pipeline kiểu Intent**: `marker ok:` → verify tầng 0 → **system-commit** (scheduler tự commit thay đổi worker bỏ sót) → tầng 1 (nếu có) → local merge. Merge là bước cuối của chuỗi kiểm. (Hiện thực OM-10 đặt tầng 1 SAU system-commit để diff được chấm chứa cả công việc được rescue.)
- **Retry with feedback**: FAIL → re-dispatch worker kèm feedback verdict; `max_verify_rounds` mặc định 2; hết budget → needs_human (*thiết kế gốc; hiện thực OM-10 fail với kind `verify_budget_exhausted`, xem divergence bên dưới*).
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
- **Wave 1**: OM-13 **DONE** (commit 8cfddb1), OM-8 **DONE** — config v2 named providers + roles (commit 188025e).
- **Wave 2**: OM-9 **DONE** — verify tầng 0 + merge pipeline + system-commit (commit f355bb5). OM-10 **DONE** — verifier LLM tầng 1 + retry budget (commit 45c6fa5 wave 1 store/config + verifier package; 76f74f4 wave 2 planner/scheduler; verifier độc lập approve cả 2 wave, confidence High).
- **Wave 3**: OM-11 **code DONE** (0e1f417 aoclient config Get/Update + 6e241ce autonomy knob/ensureAutonomy; E2E lần 6) — **caveat: zero-touch CHƯA đạt**: classifier của nấc `auto` chặn cả lệnh vô hại (`git add`, `git config`) → vẫn cần bấm tay; `accept-edits` hỏi mọi bash compound. Cơ chế set permission PASS, trust-dialog đã biến mất; zero-touch thật chờ `bypass-permissions` (opt-in + sandbox) hoặc classifier cải thiện. OM-12 chưa bắt đầu.
- **Wave 4** (OM-14) chưa bắt đầu.
- **Bug mở (phát hiện E2E lần 6, chưa vá)**: planner sinh check tier-0 đòi `git status --porcelain` sạch, nhưng marker protocol bắt worker để `.om-done.<hex8>` uncommitted → check fail 100% mọi round → `verify_budget_exhausted` oan. Cần quyết định: sửa planner prompt, hoặc runCheck loại marker trước khi chạy. Chi tiết brain/docs/e2e.md lần 6.

#### OM-11 — những gì đã hiện thực
- **4 nấc autonomy** (config `autonomy`, env `OVERMIND_AUTONOMY`, override `om run --autonomy=…`): `auto` (mặc định) | `accept-edits` | `bypass-permissions` | `off` (không đụng project config).
- **ensureAutonomy trong `om run` trước dispatch**: GET AO project config → set `agentConfig.permissions` → PUT (PUT là REPLACE toàn config nên round-trip lossless — giữ nguyên field khác); idempotent (đã đúng thì không PUT).
- **Guard bypass**: `bypass-permissions` bị từ chối khi `autonomy_allow_bypass: false` (mặc định) — worker chạy không sandbox.
- Thay thế hoàn toàn `.claude/settings.json` allowlist tĩnh (lần 3–5) — repo test lần 6 không có file này và không còn trust-folder dialog.

#### OM-10 — những gì đã hiện thực
- **Pipeline sau `ok:` marker**: `verify tầng 0 → system-commit → tầng 1 verifier LLM (chỉ task verify: llm) → local merge`. Tầng 1 chạy SAU system-commit để diff được chấm chứa cả công việc được rescue.
- **Verifier tầng 1**: LLM call trực tiếp từ scheduler qua `roles.verifier` (không tạo AO session); diff ba-chấm (`gitops.DiffText`, cap **100KB** + notice khi cắt); verdict YAML binary `ok|fail` + reason + feedback có cấu trúc, parse chịu lỗi (bóc code fence, retry 1 lần). Plan có task `verify: llm` mà config thiếu `roles.verifier` → `om run` fail sớm với lỗi rõ.
- **Retry with feedback**: verify fail (tầng 0 lẫn tầng 1) → kill session + event `task_retry {round, tier, reason, feedback}` → task về pending, re-dispatch session MỚI với prompt = prompt gốc + khối feedback + footer (tổng ≤ 4096 bytes; nếu chỗ cho feedback < 100 bytes thì thay bằng dòng ngắn `verification failed: <reason>`); displayName kèm round để dispatch idempotent per-round. Budget `max_verify_rounds` (mặc định 2) **đếm từ event log khi replay** — kill-resume không reset budget. Hết budget → `task_failed kind=verify_budget_exhausted`, nhánh khác vẫn chạy. Lỗi LLM transport không đốt budget (retry poll, 3 lần liên tiếp → `verify_error`).
- **Schema/events mới**: event `task_retry`; event `task_verdict` thêm `tier: 1`; cột `tasks.verify` (planner gán `none|deterministic|llm` per task); config `max_verify_rounds` (env `OVERMIND_MAX_VERIFY_ROUNDS`).
- **2 divergence CÓ CHỦ ĐÍCH so với thiết kế gốc ở trên**:
  1. Verdict tầng 1 audit bằng event `task_verdict {tier: 1}` thay vì file `.om-verdict.<hex8>` — event log là audit trail đầy đủ và rẻ hơn (không cần agent/worktree ghi file).
  2. Hết budget → `task_failed kind=verify_budget_exhausted` thay vì `needs_human` — session lúc đó đã terminated, `needs_human` sẽ loop vô hạn trong pollTask và chưa có đường human-unblock cho session chết; nâng cấp sau khi có human-in-the-loop resume.

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


## Deep-research: state-of-the-art multi-agent coding orchestration (2026-07-21)

Kết quả nghiên cứu internet có kiểm chứng đối kháng (mỗi claim bị 3 verifier độc lập tìm cách bác bỏ, ≥2/3 phiếu bác thì loại; nguồn đều được fetch trực tiếp ngày 21/07/2026). 10 kết luận sống sót, nhóm theo tác động vào Overmind:

### Verification (→ OM-10)
1. **Trained critic + best-of-N là pattern verify mạnh nhất đã chứng minh** (confidence: high): OpenHands đạt 66.4% SWE-bench Verified (từ 60.6% single-rollout) bằng 5 rollouts + critic model fine-tuned (Qwen 2.5 Coder 32B, regression head, reward từ unit-test pass) chọn trajectory tốt nhất — KHÔNG đổi scaffold. Họ thấy cách này hiệu quả hơn LLM-as-judge dạng prompt (generalize kém ngoài benchmark). Chi phí ~5x inference cho ~6 điểm, scale log-linear. [Nguồn](https://www.openhands.dev/blog/sota-on-swe-bench-verified-with-inference-time-scaling-and-critic-model)
   - **Ý nghĩa cho OM-10**: verifier LLM dạng prompt (thiết kế hiện tại) là bước đúng nhưng đừng kỳ vọng quá — ưu tiên tầng 0 deterministic (test-based) làm trụ, LLM verdict chỉ bổ trợ. Best-of-N per-task là hướng nâng cấp sau (đắt 5x).
2. **UI-orchestrator (vibe-kanban, crystal) verify thuần human-in-the-loop** (high): diff review tay + run script tay, không verifier LLM, không test gating. → Tier-0 verify hiện tại của Overmind đã VƯỢT nhóm này; hướng phải đuổi là critic model + merge-queue gates, không phải ngang hàng UI tools.

### Merge/integration (→ Phase 3, diamond DAG)
3. **Gas Town gộp verify + integration vào một cơ chế: merge queue kiểu Bors có bisect** (high): batch các MR, chạy verification gate trên stack đã merge; xanh → merge cả batch; đỏ → bisect cô lập MR hỏng, land phần tốt; agent không bao giờ push thẳng main; MR fail được fix inline hoặc re-dispatch. Đây là thiết kế merge-queue khớp nhất với pipeline tier-0-verify-then-merge của Overmind. Caveat: là thiết kế của tác giả (Steve Yegge), chưa có benchmark độc lập. [gastown](https://github.com/steveyegge/gastown)
4. **Worktree isolation là pattern hội tụ toàn ngành** (high): gastown (hooks sống sót crash), vibe-kanban (workspace = worktree + branch + terminal), crystal, và Claude Code native (`claude --worktree`). AO đã cho Overmind cái này miễn phí — đúng hướng.
5. **Ngoài Gas Town, KHÔNG ai tự động merge-back** (medium): vibe-kanban đi đường PR + người review; crystal squash-rebase tay; docs worktree của Claude Code không nói gì về merge-back. → **Local merge tự động của Overmind là differentiator thật**; lộ trình tiến hóa: merge tuần tự hiện tại → batched/bisecting queue kiểu Refinery khi mở diamond DAG.

### Autonomy/permissions (→ OM-11) — toàn bộ từ docs chính thức Claude Code, verify nguyên văn
6. **`--dangerously-skip-permissions` PHẢI nằm trong container/VM/sandbox** (high): khi tắt prompt, isolation boundary là lớp bảo vệ duy nhất. Reference pattern chính thức: dev container + iptables egress default-deny + non-root (flag bị chặn khi chạy root/sudo; check này tự bỏ qua trong sandbox được nhận diện). [sandbox-environments](https://code.claude.com/docs/en/sandbox-environments)
7. **"Auto mode" là lựa chọn mới nhẹ hơn skip-permissions** (high): thay prompt bằng classifier per-action (chặn escalation, target lạ, hành động do content độc điều khiển); isolation được khuyến nghị nhưng không bắt buộc. → Ứng viên tốt cho OM-11 qua AO `--permission-mode`.
8. **Sandboxed Bash tool (Seatbelt/bubblewrap) chỉ trói Bash** (high): MCP server/hooks chạy tự do trên host → một mình nó KHÔNG đủ cho unattended. Có escape hatch documented (retry với `dangerouslyDisableSandbox`) — orchestrator phải khóa bằng `"allowUnsandboxedCommands": false`.
9. **Lỗ hổng exfiltration documented**: proxy allowlist của sandbox không terminate TLS mặc định → domain fronting thoát được allowlist; entry rộng (github.com) tự nó là đường exfil. Threat model mạnh cần TLS-terminating proxy (`network.tlsTerminate`, experimental v2.1.199+). [sandboxing](https://code.claude.com/docs/en/sandboxing)
10. **3 cơ chế permission phân bậc cho headless `claude -p`** (high): (a) `--allowedTools` với prefix rule (`Bash(git diff *)`); (b) `acceptEdits` — auto-approve write + mkdir/touch/mv/cp, bash/network khác vẫn cần allow rule, **không có thì run non-interactive ABORT**; (c) `dontAsk` — deny-by-default cho CI khóa chặt. Abort-on-unapproved là failure mode cụ thể mà crash-resume của Overmind phải xử lý (scheduler sẽ thấy session chết không marker → hiện ra là `marker_missing`/`no_signal`). [headless](https://code.claude.com/docs/en/headless)

### Khuyến nghị rút ra cho Phase 2 (thứ tự hành động)
- **OM-11**: chọn 2 nấc qua AO project config (đã verify PUT hoạt động ở E2E lần 2) — *hiện thực đã chốt default `auto`, `accept-edits` là nấc hạ*; nấc full-auto (`bypass-permissions`) CHỈ khi session nằm trong sandbox-runtime/container, khóa `allowUnsandboxedCommands:false`, và hiểu giới hạn TLS/domain-fronting nếu bật network.
- **OM-10**: giữ tier 0 (test/check-based) làm cổng chính; verifier LLM prompt-based là tín hiệu bổ trợ, không phải cổng duy nhất — bằng chứng OpenHands cho thấy prompt-judge generalize kém.
- **Phase 3 merge queue**: học Refinery của gastown (đã có source trong docs/repo/) — batch + test-merged-stack + bisect; đó là điều kiện mở diamond DAG.
- **2 câu hỏi chưa có kết luận sống sót qua kiểm chứng** (cần nghiên cứu tiếp): re-planning/budget control giữa chừng, và postmortem thất bại công khai của các hệ orchestrator.

## Checklist dựng lại môi trường trên máy mới (2026-07-21)

### 1. Clone repo
`git clone https://github.com/OmniMintX/Agents-OmniMintX.git`
Có sẵn: brain/ (code), SPEC.md, docs/OVERMIND.md, brain/docs/e2e.md, brain/config.example.yaml.

### 2. Toolchain
- Go >= 1.25 (go.mod). Xác nhận môi trường: `cd brain && go test ./... -race -count=1` (mốc: 149 pass / 7 packages tại e3a0395).
- git, tmux (worker session chạy trong tmux).

### 3. AO (agent-orchestrator) — bắt buộc cho runtime/E2E
- Cài app AO (bản đang dùng 0.10.3), daemon nghe port 3001.
- ~/.ao/data/ cũ KHÔNG cần mang theo (state toàn plan test /tmp).
- Harness: Claude Code CLI + đăng nhập.

### 4. Config Overmind
- ~/.overmind/ tự tạo khi chạy; máy cũ không có config.yaml (chạy default + env OVERMIND_*) — không có gì phải chép.
- ~/.overmind/overmind.db cũ không cần mang.
- LLM remote: set env API key theo api_key_env (vd ANTHROPIC_API_KEY).

### 5. docs/repo/ — repo tham khảo (gitignore, không theo repo)
14 repo bổ sung ưu tiên: xem mục "Repo tham khảo cần bổ sung" phía trên. 26 repo hiện có trên máy cũ:

```bash
cd docs/repo
git clone https://github.com/saltbo/agent-kanban.git
git clone https://github.com/AgentWrapper/agent-orchestrator.git
git clone https://github.com/aannoo/awesome-agent-orchestrators.git
git clone https://github.com/steveyegge/beads
git clone https://github.com/ruvnet/claude-flow
git clone https://github.com/smtg-ai/claude-squad.git
git clone https://github.com/dagger/container-use
git clone https://github.com/stravu/crystal.git
git clone https://github.com/standardagents/dmux.git
git clone https://github.com/automagik-dev/forge.git
git clone https://github.com/knowsuchagency/fulcrum.git
git clone https://github.com/steveyegge/gastown
git clone https://github.com/langchain-ai/langgraph
git clone https://github.com/letta-ai/letta
git clone https://github.com/mem0ai/mem0
git clone https://github.com/alvinunreal/oh-my-opencode-slim
git clone https://github.com/open-multi-agent/open-multi-agent
git clone https://github.com/sst/opencode
git clone https://github.com/OpenHands/OpenHands
git clone https://github.com/nikitavivat/Overseer
git clone https://github.com/vidhatanand/overstory.git
git clone https://github.com/johannesjo/parallel-code.git
git clone https://github.com/CChuYong/rookery.git
git clone https://github.com/delivstat/swarmkit
git clone https://github.com/SWE-agent/SWE-agent
git clone https://github.com/BloopAI/vibe-kanban.git
```

### 6. Workspace Intent
- Spec + task notes Phase 2 nằm trong workspace agent-orchestrator — mở lại là có.
- OM-10 (verifier LLM + retry budget) đã xong và merge (45c6fa5 + 76f74f4); OM-11 (autonomy, default `auto`) code xong (0e1f417 + 6e241ce); việc kế tiếp là OM-12 (approval gates) + bug planner-check-vs-marker (xem Trạng thái hiện tại).
