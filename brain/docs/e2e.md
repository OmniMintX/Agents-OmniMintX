# E2E: chạy 1 goal thật qua AO daemon

Kịch bản chạy tay từng bước + kết quả kỳ vọng, và nhật ký chạy THẬT (kể cả fail/blocked).

## Điều kiện tiên quyết (GATE)

1. AO daemon đang chạy: `cat ~/.ao/running.json` (pid phải còn sống) và
   `curl http://127.0.0.1:3001/api/v1/projects` trả 200.
   - AO là app desktop (`/Applications/Agent Orchestrator.app`); app tự chạy daemon.
     Nếu daemon chết: `open -a "Agent Orchestrator"` rồi poll lại endpoint trên.
   - Lưu ý: `running.json` có thể STALE (pid đã chết nhưng file còn) → luôn kiểm tra
     bằng HTTP, không tin file.
2. Planner LLM: mặc định dùng `claude` CLI (`provider=cli`) — chỉ cần `which claude`
   trả về binary. KHÔNG cần `ANTHROPIC_API_KEY` trong env; chỉ cần key khi cấu hình
   `provider=anthropic` (gọi Messages API trực tiếp).
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

---

## Nhật ký chạy thật — lần 2, 2026-07-20 → 21 (AO 0.10.3, planner cli/claude)

**Verdict: FAIL** — pipeline Overmind chạy hết A→B và plan "done", nhưng
**chaining VỠ đúng như RISK dự đoán**: B không thấy code của A. Chi tiết dưới.

Provider/model: planner `provider=cli` (binary `claude`); worker harness
`claude-code` qua AO. AO daemon 0.10.3, port 3001.

### Unblock API key — DONE
Planner thêm `provider=cli` gọi `claude` CLI thay vì Messages API → GATE không
cần `ANTHROPIC_API_KEY` nữa (đã cập nhật mục GATE ở trên).

### Bug Overmind phát hiện & fix trong lần 2 (đều có unit test, go test -race sạch)
1. **Completion detection vỡ trên AO 0.10.3**: Overmind kiểm `.om-done` qua
   `GET /sessions/{id}/workspace/files` nhưng AO 0.10.3 KHÔNG có route đó
   (ROUTE_NOT_FOUND) → task xong việc mà Overmind không bao giờ thấy marker.
   Fix: `aoclient.PreviewFile` (route `/preview/files/{path}` — có thật, chính là
   `previewUrl` AO trả về) + scheduler fallback khi listing 404. Test:
   `TestPreviewFile*` (aoclient), `TestRunDoneMarkerPreviewFallback` (scheduler).
2. **Task id không plan-scoped trong SQLite**: planner sinh `t1..tN` cho MỌI plan,
   nhưng `tasks.id` là PK toàn cục → plan thứ 2 trở đi fail
   "UNIQUE constraint failed: tasks.id". `task_dependencies` cũng dính PK
   `(task_id, depends_on_task_id)` không có plan_id. Fix: PK composite
   `(id, plan_id)` / `(plan_id, task_id, depends_on_task_id)` + migration rebuild
   tables cho DB cũ (`migrateTasksPK`, chạy trong `store.Open`). Test:
   `TestCreatePlanReusesTaskIDsAcrossPlans`, `TestMigrateTasksPK`.

### Kịch bản chính A→B — plan p-a246ce32 (00:01→00:05, ~4 phút)
- t1 (om-e2e-1784566150-2): tạo `greeting.txt`, commit a71108c trên branch
  `ao/om-e2e-1784566150-2/root`, tạo `.om-done`. Overmind detect qua fallback
  preview → `task t1: DONE (pr: )` — **PR rỗng**.
- **AO worker KHÔNG tự mở PR** (câu hỏi RISK — trả lời thực nghiệm: KHÔNG).
  Session không có PR nào; AO 0.10.3 cũng không có API tạo PR cho session
  (`/sessions/{id}/prs` = ROUTE_NOT_FOUND). Repo local không có remote → không
  thể có PR đúng nghĩa.
- Hệ quả: `ensureParentsMerged` thấy 0 PR → coi như "đã merge" → dispatch t2 từ
  `main` (chỉ có README). t2 (om-e2e-1784566150-3) báo đúng sự thật:
  "greeting.txt is not in my worktree, but it exists on sibling branches",
  không tạo `reply.txt`, ghi `.om-done` nội dung **failure**.
- **Overmind vẫn kết luận `task t2: DONE` và `plan done`** vì tiêu chí done chỉ là
  "idle + tồn tại .om-done", không đọc nội dung marker. `main` cuối cùng KHÔNG có
  `greeting.txt` lẫn `reply.txt` → E2E chaining **FAIL**, không phải "xanh giả".
- Timeline events đầy đủ đúng thứ tự: plan_created→approved→run_started→
  dispatching/dispatched→needs_human×2 (permission prompts)→resumed→done×2→plan_done.

### Kịch bản phụ resume — plan p-5fd37286: PASS
- `om run` bị kill -9 giữa lúc t1 needs_input → chạy lại.
- Lần chạy lại đầu trong <60s bị chặn đúng: "plan p-5fd37286 is locked by another
  om run (pid 73985)" (lock stale sau 60s — by design, không phải bug).
- Sau 60s: run mới adopt lại session `om-e2e-1784566150-4` theo displayName marker,
  **không tạo session trùng** (count sessions của project giữ nguyên 4), resume
  needs_input → done. Events cho thấy 2 run_started nhưng chỉ 1 task_dispatched.

### Phát hiện quan trọng (không fix — lỗi thiết kế, cần quyết định)
1. **PR-chaining VỠ trên AO 0.10.3 + repo local**: worker không mở PR, AO không có
   API PR per-session, repo không remote. `ensureParentsMerged` coi 0 PR = merged
   là sai với thực tế này. Đề xuất: (a) dispatch task con với base = branch của
   cha (`ao/{parent-session}/root`) thay vì default branch, hoặc (b) scheduler tự
   merge branch cha vào main local trước khi dispatch con. Cần quyết định thiết kế.
2. **"done" quá dễ dãi**: chỉ cần idle + `.om-done` tồn tại. Agent ghi marker nội
   dung "failure: ..." vẫn thành done. Đề xuất: định dạng marker có cấu trúc
   (vd dòng đầu `ok|fail`) — cần sửa cả prompt.tmpl lẫn scheduler, để bàn.
3. **Permission prompts của claude-code**: mỗi Bash/Write đều needs_input; Overmind
   pause timeout đúng thiết kế nhưng E2E cần người bấm approve trong tmux.
   Phân tích đường truyền permission (từ source AO trong docs/repo/, verify live
   trên daemon 0.10.3) — input cho Phase 2:
   - `POST /api/v1/sessions` (SpawnSessionRequest) KHÔNG có field permissions —
     xác nhận lại kết luận grep trước: không truyền được cờ per-spawn.
   - NHƯNG AO có cấu hình per-project: `PUT /api/v1/projects/{id}/config` với
     `{"config":{"agentConfig":{"permissions":"accept-edits|auto|bypass-permissions"}}}`
     (domain.ProjectConfig.AgentConfig + role override `worker.agentConfig`).
     Session manager resolve config này TẠI spawn (`effectiveAgentConfig`), adapter
     claude-code map thành `--permission-mode acceptEdits|auto|bypassPermissions`
     (bypass ≡ `--dangerously-skip-permissions`). Đã verify live: PUT trả 200 và
     config persist. → Phương án (a) KHẢ THI: Overmind set project config 1 lần
     khi đăng ký project; session spawn sau đó tự có flag, không cần người bấm.
   - Phương án (b) cũng khả thi: commit `.claude/settings.json` với
     `permissions.allow` vào repo dự án — worktree AO thừa hưởng theo repo.
     Nhược: đổi nội dung repo của user, allowlist phải liệt kê từng tool/pattern.
   - Đề xuất Phase 2: (a) là chính — Overmind PUT `worker.agentConfig.permissions`
     = `accept-edits` (đủ cho edit+safe bash, vẫn chặn network/system bash) hoặc
     `auto`; `bypass-permissions` chỉ khi user opt-in rõ ràng. (b) làm fallback
     cho harness không hỗ trợ permission-mode. Lưu ý: config áp dụng cho session
     spawn MỚI, không ảnh hưởng session đang chạy.
4. AO daemon chết giữa chừng 1 lần (trước khi plan p-a246ce32 được tạo);
   `open -a "Agent Orchestrator"` + poll HTTP khôi phục trong ~2s. Overmind báo lỗi
   rõ ràng, không hỏng state.


---

## Nhật ký chạy thật — lần 3, 2026-07-21 (AO 0.10.3, om build 65242f4)

**Verdict: PASS** — chaining sống thật: local merge hoạt động, B đọc được file
của A từ `main`; marker có cấu trúc phân biệt được ok/fail; resume adopt không
dispatch trùng. Chi tiết + caveat bên dưới.

Thiết kế được verify: local git merge thay PR (gitops.Merger), marker
`.om-done.<hex8>` + honesty footer inject lúc dispatch, verdict 5 nhánh,
`.claude/settings.json` autonomy commit sẵn vào repo test.

### Hiện trường cũ p-f55373e1 (repo 1784570341): FAIL — no_signal
- t1 dispatch xong nhưng session kẹt ở dialog "This folder pre-approves 11 tool
  permissions… Do you trust?" của claude-code (dialog TRUST FOLDER do chính
  `.claude/settings.json` gây ra, xuất hiện TRƯỚC cả prompt đầu tiên).
- Không ai bấm → `no_signal` >10m → scheduler kill session + task_failed
  `kind=no_signal` + plan_failed. Đúng thiết kế timeout, nhưng hiện trường
  không cứu được (session terminated) → làm lại repo mới.
- Phát hiện phụ: process `om run` (pid 92677) SỐNG SÓT khi terminal cha chết;
  lock plan theo pid chặn đúng run thứ 2 ("locked by another om run").

### Kịch bản chính A→B — plan p-0359a657 (repo /tmp/om-e2e-1784571147): PASS
- Repo mới: `.claude/settings.json` (acceptEdits + allowlist + deny) commit
  TRƯỚC khi plan. Plan t1→t2, approve, `om run`.
- t1 (om-e2e-1784571147-1): vẫn kẹt trust-folder dialog → needs_human; bấm
  Enter thủ công trong tmux 1 lần → resumed. Sau đó agent chạy `xxd` (không có
  trong allowlist) → thêm 1 prompt approval nữa (bấm "don't ask again for xxd").
  Agent commit `greeting.txt` (d0dac6f), ghi marker `.om-done.e4cc4a97` nội dung
  `ok: …` (honesty footer hiển thị đúng trong system prompt của worker — thấy
  nguyên văn trong tmux).
- Scheduler: verdict ok → **local merge** `ao/…-1/root` vào main (08ea152,
  event `task_branch_merged`) → FinishTask → dispatch t2.
- t2 (om-e2e-1784571147-2): chạy **không cần bấm gì** (trust đã có, không dùng
  lệnh ngoài allowlist). Đọc được `greeting.txt` từ main thật, tạo `reply.txt`
  = `reply to: hello-om-e2e-1784571147` (TRÍCH đúng nội dung A), commit, marker
  `ok:` → merge 113dac5 → plan done.
- Verify: `git show main:greeting.txt` = `hello-om-e2e-1784571147`;
  `git show main:reply.txt` = `reply to: hello-om-e2e-1784571147`. Events đúng
  thứ tự: …→task_branch_merged(t1)→task_done(t1)→dispatched(t2)→
  task_branch_merged(t2)→task_done(t2)→plan_done. Payload task_done giờ có
  `{"marker":"ok","summary":…}`.

### Kịch bản fail thật — plan p-b532813d: PASS
- Goal: task PHẢI báo fail (đọc `/nonexistent-xyz-e2e`, không được tạo file).
- Agent ghi marker `fail: /nonexistent-xyz-e2e does not exist; out.txt not
  written` → scheduler: task_failed `kind=marker_fail` kèm `marker_content`,
  **KHÔNG merge** (branch `ao/…-3/root` vẫn trỏ main cũ, không có event
  task_branch_merged), main không đổi (113dac5 trước == sau). Plan failed.
- Đây chính là lỗ hổng "xanh giả" của lần 2 — giờ bị chặn đúng.

### Kịch bản resume — plan p-fe1fb0e3: PASS
- Run 1 (run-d58840548e4a) dispatch t1 (session om-e2e-1784571147-4), chết giữa
  lúc t1 needs_input. Run 2 (run-e8c5f0ae8188) **adopt** session theo
  displayName: events có 2 `run_started` nhưng chỉ 1 `task_dispatched`,
  session count của project giữ nguyên (không spawn trùng). Resume → marker ok
  → merge 712d248 → done.

### Bug stale lock phát hiện & fix trong lần 3 (commit cdcb947)
- **Bug**: `om run` bị kill -9 để lại `run_lock_pid` trong SQLite; run mới bị
  từ chối "locked by another om run (pid N)" dù pid N đã chết — resume bị chặn
  đến hết cửa sổ LockStaleAfter (60s). Với kill ngay sau heartbeat, đây là chờ
  vô ích; và người dùng không phân biệt được "đang chạy thật" với "xác chết".
- **Fix**: `AcquireRunLock` kiểm tra holder còn sống (`kill(pid,0)`, EPERM =
  sống) khi UPDATE thường không ăn; nếu holder chết → CAS đúng pid holder để
  steal (2 kẻ steal đồng thời chỉ 1 thắng), trả `tookOver=true` → scheduler
  log "previous om run holder is dead — taking over its run lock".
- Unit test: `TestRunLockDeadHolder` (pid sống → từ chối, pid chết → tiếp quản
  + holder mới ghi đúng), `TestRunLock`/`TestRunLockContention` cập nhật theo
  ngữ nghĩa mới. go vet + go test ./... -race sạch.
- **Verify live** (plan p-8b0b3406, cùng repo): run 1 (pid 52997) dispatch t1
  → kill -9 → run 2 CHẠY NGAY (không chờ 60s), log takeover, **adopt** session
  om-e2e-1784571147-6 (events: 2 run_started, chỉ 1 task_dispatched; session
  count của project 6 = không spawn trùng) → marker ok → merge 4a77fb3 →
  `git show main:lock-test2.txt` = `lock-ok-2`. PASS.

### Phát hiện quan trọng lần 3
1. **Trust-folder dialog là needs_input mới**: chính `.claude/settings.json`
   (giải pháp autonomy) lại sinh ra dialog "do you trust this folder" ở lần
   spawn đầu trong worktree mới → t1 của mỗi plan đầu vẫn cần 1 lần bấm tay.
   Sau khi trust, các session sau trong CÙNG repo không hỏi lại (t2 chạy sạch).
   → Phase 2 nên dùng phương án PUT project config `worker.agentConfig
   .permissions` (đã verify lần 2) thay vì/kết hợp settings.json.
2. **Allowlist không đủ**: agent tự chọn lệnh ngoài allowlist (`xxd`) → vẫn
   prompt. acceptEdits chỉ cover Write/Edit; bash lạ luôn hỏi. Không coi
   allowlist tĩnh là đủ cho autonomy.
3. **Chuỗi merge-trước-dispatch hoạt động đúng như thiết kế**: verdict ok →
   merge local → FinishTask → con dispatch từ main đã chứa code cha. Fail
   marker → không merge, main sạch. Timeout no_signal kill đúng.
