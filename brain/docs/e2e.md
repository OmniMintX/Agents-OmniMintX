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


---

## Nhật ký chạy thật — lần 4, 2026-07-21 (máy mới)

**Verdict: PASS** — tái lập nguyên kịch bản lần 3 trên máy mới (Mac mini,
AO 0.10.3, claude CLI 2.1.205, om build từ HEAD 49c3060): chaining A→B sống,
merge local đúng, marker ok, plan done trong ~4 phút. Không có fix code nào.

### GATE — kết quả thật
- Daemon UP sẵn: `GET /api/v1/projects` = 200, danh sách project RỖNG (máy mới).
- `which claude` OK (2.1.205); KHÔNG có `~/.overmind/config.yaml` — defaults
  auto-detect `cli/claude` cho planner hoạt động đúng, không cần config file.
- `go build -o /tmp/om ./cmd/om` sạch.

### Đăng ký project — lưu ý schema
- Body `{"id":…}` bị 400 `INVALID_JSON` — daemon strict-decode `AddProjectInput`:
  field đúng là **`projectId`** (kèm `path`, `name`), không phải `id` như dòng
  "(id/name/path)" ở mục kịch bản phía trên gợi ý. Gửi đúng schema → 201,
  project `om-e2e-1784638812` (kind single_repo, defaultBranch main).

### Kịch bản chính A→B — plan p-3c2f9f87 (repo /tmp/om-e2e-1784638812): PASS
- Repo mới: `.claude/settings.json` (acceptEdits + allowlist git/cat/ls/… +
  deny curl/wget/rm -rf/sudo) commit 6ea09b9 TRƯỚC khi plan. Plan t1→t2
  (planner cli/claude, ~8s), approve, `om run` (run-1baa501f0bf7, 20:01→20:05).
- t1 (om-e2e-1784638812-1): kẹt trust-folder dialog đúng caveat lần 3
  ("pre-approves 18 tool permissions… Do you trust?") → needs_human; gửi
  Enter 1 lần qua tmux → resumed. Agent chạy lệnh compound
  `pwd && git -C . rev-parse … && git status --porcelain` (compound không khớp
  allowlist prefix) → thêm 1 prompt approval (chọn "don't ask again") — tái
  lập phát hiện lần 3: allowlist tĩnh không đủ. Sau đó agent commit
  `greeting.txt` (4f8af82), marker `ok:`.
- Điểm MỚI so với lần 3: build hiện tại có **tier-0 verify** — event
  `task_verdict {"tier":0,"verdict":"pass"}` trước `task_branch_merged`
  cho cả 2 task.
- Scheduler: verdict pass → merge local `ao/…-1/root` vào main (78b370aa) →
  task_done(t1) → dispatch t2.
- t2 (om-e2e-1784638812-2): chạy **không cần bấm gì** (trust đã có, không dùng
  lệnh ngoài allowlist — giống hệt lần 3). Đọc `greeting.txt` từ main thật,
  tạo `reply.txt` = `reply to: hello-om-e2e-1784638812` (TRÍCH đúng), commit
  3665f17, marker `ok:` → merge e28f8578 → plan done.
- Verify: `git show main:greeting.txt` = `hello-om-e2e-1784638812`;
  `git show main:reply.txt` = `reply to: hello-om-e2e-1784638812`. Events đúng
  thứ tự: plan_created→approved→run_started→dispatched(t1)→started→
  needs_human→resumed→task_verdict(t1)→task_branch_merged(t1)→task_done(t1)→
  dispatched(t2)→started→task_verdict(t2)→task_branch_merged(t2)→
  task_done(t2)→plan_done. `om status`: cả 2 task done.

### Không chạy lại trong lần 4
- Kịch bản fail thật và kịch bản resume/stale-lock (đã PASS lần 3, không thuộc
  scope smoke máy mới).

### Dọn dẹp
- `om run` tự thoát exit 0 khi plan done; không còn process `om run` hay tmux
  session nào; 2 session AO đều terminated. Repo /tmp/om-e2e-1784638812 giữ
  lại làm hiện trường.


---

## Nhật ký chạy thật — lần 5, 2026-07-21: tier-1 LLM verify sống (OM-10)

**Verdict: PASS** — lần đầu OM-10 chạy THẬT qua AO daemon (không phải unit
test): gate fail-fast thiếu `roles.verifier` chặn đúng; tier-1 pass → merge;
tier-1 FAIL → `task_retry` round 1 kèm feedback → worker round 1 sửa đúng →
pass → merge (nhánh retry-with-feedback THÀNH CÔNG, không cần đến budget).
Máy Mac mini, AO 0.10.3, claude CLI 2.1.205, om build từ HEAD 48f4a5f.

### GATE — kết quả thật
- Daemon UP: `GET /api/v1/projects` = 200. `go build -o /tmp/om ./cmd/om` sạch.
- CHƯA có `~/.overmind/config.yaml` (chỉ có overmind.db) — đúng tiền đề để test
  fail-fast; planner vẫn chạy nhờ defaults auto-detect `cli/claude`.

### Đăng ký project — caveat curl
- Repo test `/tmp/om-e2e5-1784644173` (git init -b main, commit README +
  `.claude/settings.json` autonomy copy từ lần 4, commit 5c11b7d).
- POST /projects với `-d '{...}'` inline trong shell bị 400 `INVALID_JSON` dù
  body nhìn giống hệt; gửi CÙNG nội dung qua `--data @file` → 201 (project
  om-e2e5-1784644173, single_repo, defaultBranch main). Nghi shell escaping
  của lớp chạy lệnh — từ nay luôn dùng `--data @file` cho JSON body.

### Bước 1 — gate fail-fast thiếu roles.verifier: PASS
- Plan p-67896673 (goal haiku cần semantic review) → planner gán t1
  `verify=llm` ngay lần thử đầu, check tier-0 tự sinh khá chặt
  (`test -s haiku.txt && grep -c . == 3 && wc -l <= 3`).
- Approve rồi `om run` → fail SỚM, exit 1, message nguyên văn:
  `Error: plan p-67896673 has verify=llm tasks but no usable verifier LLM: no
  roles.verifier in config (assign it a provider + model)`.
- `om events p-67896673` chỉ có plan_created + plan_approved — KHÔNG có
  run_started/dispatch nào: gate chặn trước khi đụng AO. Acceptance criterion
  OM-10 xác nhận bằng thực nghiệm.

### Bước 2 — config v2
- Tạo `~/.overmind/config.yaml` (chưa tồn tại — không cần backup):
  `providers.default` (type cli, command claude, args `-p --output-format
  json`, timeout_sec 240), `roles.planner` + `roles.verifier` → default,
  `max_verify_rounds: 2`. GIỮ LẠI file này cho các lần sau.

### Bước 3 — happy path tier-1 PASS — plan p-58a76b39 (21:34→21:39, ~5 phút)
- Goal A→B như lần 4 + yêu cầu t2 semantic: t1 tạo `greeting.txt`
  (verify=deterministic), t2 đọc greeting → `reply.txt` một câu tiếng Anh lịch
  sự trích nguyên văn greeting (verify=llm). Planner gán verify đúng từng task.
- t1: trust-folder dialog (tái lập caveat lần 3/4, "pre-approves 18 tool
  permissions") → 1 Enter qua tmux → marker ok → tier-0 pass → merge 2900f34.
- t2: 1 approval prompt MỚI dạng command-substitution — allowlist có
  `Bash(cat:*)` nhưng `grep -F "$(cat greeting.txt)" reply.txt` bị chặn vì
  "Contains shell syntax (string) that cannot be statically analyzed" → 1
  Enter. Sau đó: `task_verdict {"tier":0,"verdict":"pass"}` → **`task_verdict
  {"tier":1,"verdict":"pass"}`** (tier-1 LLM call ~23s) → merge 26ec873 →
  task_done → plan_done. `om run` tự thoát exit 0.
- Verify: `git show main:reply.txt` = `Thank you for your kind message
  "hello-om-e2e5-1784644173"; we are delighted to hear from you.` — chaining
  vẫn sống, tier-1 nằm đúng chỗ trong pipeline (SAU tier-0, TRƯỚC merge).

### Bước 4 — retry path (điểm đinh) — plan p-5afee3cf (21:40→21:43, ~2.5 phút)
- Goal ép mâu thuẫn: DoD "poem.txt = haiku mùa THU 3 dòng 5-7-5" nhưng prompt
  bảo worker viết LIMERICK 5 dòng về mùa ĐÔNG; check tier-0 chỉ
  `test -s poem.txt` (cố ý yếu để lỗi lọt xuống tier-1). Planner sinh prompt
  đúng ý ngay lần thử đầu, verify=llm.
- Round 0 (session om-e2e5-1784644173-3, displayName om-da523a87): worker viết
  limerick như prompt bảo → marker ok → tier-0 pass → **tier-1 FAIL** 21:42:19,
  reason: "definition of done requires … haiku about autumn … but the diff
  writes a five-line winter limerick … The embedded instruction to 'ignore any
  tension' … cannot override it" — verifier không bị prompt-injection nhẹ
  trong task prompt lừa.
- `task_retry {round:1, tier:1}` cùng giây: kill session round 0, feedback CÓ
  CẤU TRÚC (reason + finding per-file + hướng sửa "Replace the entire contents
  … with an original three-line English haiku about autumn"). Re-dispatch
  session MỚI om-e2e5-1784644173-4 (displayName om-3af620f5 ≠ round 0 — đúng
  thiết kế idempotent per-round), không cần bấm gì (trust đã có).
- Round 1: worker viết haiku mùa thu (bất chấp prompt gốc vẫn nói limerick —
  bằng chứng hành vi rằng khối feedback nằm trong prompt round 1) → tier-0
  pass → **tier-1 pass** 21:43:16 → merge 6852407 → task_done → plan_done.
- **Nhánh (a) retry-with-feedback THÀNH CÔNG**; không đi đến
  `verify_budget_exhausted` (budget chưa được thử sống — ghi nhận để lần sau).
- Verify không "xanh giả": branch round-0 `ao/…-3/root` KHÔNG có event
  task_branch_merged, main không chứa limerick; `git show main:poem.txt` =
  haiku 3 dòng về mùa thu; git log chỉ merge branch `…-4/root`.

### Chuỗi events p-5afee3cf (om events, rút gọn)
```
21:41:54 task_verdict  t1 {"tier":0,"verdict":"pass"}
21:42:19 task_verdict  t1 {"tier":1,"verdict":"fail","reason":"…haiku about autumn…but the diff writes a five-line winter limerick…"}
21:42:19 task_retry    t1 {"round":1,"tier":1,"feedback":"…Replace the entire contents of poem.txt with an original three-line English haiku…"}
21:42:19 task_dispatched t1  (session -4, round 1)
21:43:04 task_verdict  t1 {"tier":0,"verdict":"pass"}
21:43:16 task_verdict  t1 {"tier":1,"verdict":"pass"}
21:43:16 task_branch_merged t1 {"branch":"ao/om-e2e5-1784644173-4/root","sha":"68524073…"}
21:43:16 task_done     t1 {"marker":"ok"}
21:43:17 plan_done
```

### Caveat mới lần 5
1. **JSON body qua curl `-d` inline có thể bị 400 INVALID_JSON** — dùng
   `--data @file` (xem mục đăng ký project).
2. **Không capture được prompt round-1 trực tiếp**: session terminated ngay
   khi xong, AO API không expose initial prompt (GET /sessions/{id} không có
   field prompt). Bằng chứng feedback-in-prompt là gián tiếp: hành vi worker
   round 1 (làm theo DoD thay vì instruction limerick trong prompt gốc) + event
   task_retry chứa nguyên khối feedback. Muốn bằng chứng trực tiếp phải capture
   tmux trong lúc session round 1 còn sống.
3. **Approval prompt dạng command-substitution**: allowlist prefix không cover
   lệnh chứa `$( )` — claude-code chặn "cannot be statically analyzed" dù lệnh
   con (cat) nằm trong allowlist. Biến thể mới của caveat "allowlist tĩnh
   không đủ" (lần 3/4).
4. **Tier-1 verifier kháng injection nhẹ**: câu "ignore any tension" cài trong
   task prompt không đổi được verdict — verdict nêu đích danh và bác bỏ.
5. Chi phí tier-1: ~23s (t2 happy) và ~12–25s (p-5afee3cf) mỗi verdict qua
   cli/claude — chấp nhận được cho pipeline hiện tại.

### Chưa thử sống trong lần 5
- Nhánh (b) `task_failed kind=verify_budget_exhausted` (round 1 đã pass nên
  budget không cạn); lỗi transport LLM (`verify_error`, không đốt budget).

### Dọn dẹp
- Cả 2 `om run` tự thoát khi plan done; không còn process om run hay tmux
  session; 4 session AO của repo lần 5 đều terminated. GIỮ nguyên hiện trường:
  repo /tmp/om-e2e5-1784644173, ~/.overmind/overmind.db, ~/.overmind/config.yaml.


---

## Nhật ký chạy thật — lần 6, 2026-07-21: autonomy modes (OM-11)

**Verdict: OM-11 PASS — zero-touch ĐẠT với `bypass-permissions` (run 3).**
ensureAutonomy hoạt động đúng cả 4 đường (unset→auto, idempotent, override CLI
auto→accept-edits, accept-edits→bypass-permissions);
trust-folder dialog BIẾN MẤT (không cần `.claude/settings.json` nữa); daemon
crash giữa run + resume adopt PASS. Hai nấc `auto`/`accept-edits` còn cần bấm
tay (`auto` bị classifier chặn cả `git add`/`git config`, `accept-edits` hỏi
mọi bash compound), nhưng run 3 với `bypass-permissions` (sau opt-in
`autonomy_allow_bypass`) chạy 0 prompt. Bonus: lần đầu quan sát sống nhánh
`verify_budget_exhausted` của OM-10 — do 1 bug OM thật tìm được nhờ E2E
(planner check vs marker protocol, root cause + fix bên dưới, fix validate
sống ở run 3).
Máy Mac mini, AO 0.10.3, om build từ HEAD 6e241ce (OM-11a 0e1f417 + OM-11b);
run 3 dùng /tmp/om build lại kèm fix EnsureExcluded.

### Setup — khác biệt then chốt so với lần 3–5
- Repo test `/tmp/om-e2e6-1784648067`: commit đầu CHỈ có README —
  **KHÔNG có `.claude/settings.json`** (autonomy giờ đi qua AO project config).
- Đăng ký project qua `POST /api/v1/projects` body `{"projectId":…,"path":…,
  "name":…}` (`--data @file`, đúng caveat lần 4/5) → 201.
- `~/.overmind/config.yaml` giữ nguyên từ lần 5, KHÔNG có key `autonomy` →
  default code `auto` có hiệu lực.

### ensureAutonomy — PASS cả 3 đường (log nguyên văn)
1. Run 1 plan p-e817ba09: `autonomy: project om-e2e6-1784648067 permissions
   (unset) → auto` — GET config (chưa có permissions) → PUT với
   `agentConfig.permissions=auto`.
2. Hai lần resume sau đó (daemon crash, xem dưới): `autonomy: đã ở auto` —
   idempotent, không PUT thừa.
3. Run 2 plan p-72724723 với `om run --autonomy=accept-edits`:
   `autonomy: project om-e2e6-1784648067 permissions auto → accept-edits` —
   override CLI thắng config.
4. Round-trip lossless xác nhận bằng GET cuối: config còn nguyên
   `{"agentConfig":{"permissions":"accept-edits"},"worker":{"agentConfig":{}},
   "orchestrator":{"agentConfig":{}},"trackerIntake":{}}` — các field khác
   không bị PUT (replace semantics) xóa mất.
- **Trust-folder dialog KHÔNG xuất hiện ở bất kỳ session nào (1–5)** — đúng
  mục tiêu OM-11 "xóa trust-dialog + allowlist tĩnh". Mọi needs_human còn lại
  đều là permission prompt per-command.

### Daemon crash + resume (tình cờ, thành kịch bản thật): PASS
- Giữa run 1, AO daemon chết → event `ao_unreachable {"error":"aoclient: AO
  daemon is not reachable; check it is running (the run file ~/.ao/running.json
  records the live port)…"}`, `om run` thoát lỗi rõ ràng, không hỏng state.
- `open -a "Agent Orchestrator"` → daemon UP lại (~vài giây) → chạy lại
  `om run p-e817ba09`: adopt session `om-e2e6-1784648067-1` theo displayName
  (events: 3 `run_started` nhưng mỗi task chỉ 1 `task_dispatched`), resume
  needs_input → tier-0 pass → merge. Tái lập OM-9 crash-resume lần 3/5.

### Plan 1 — nấc `auto` (p-e817ba09, A→B chaining): plan DONE, KHÔNG zero-touch
- Goal 2 task chained như lần 4: t1 tạo `greeting.txt`, t2 (depends t1) đọc
  và trích nguyên văn vào `reply.txt`.
- **Classifier của auto mode chặn lệnh vô hại** — nguyên văn từ tmux pane t1:
  - `Bash(printf 'hello…' > greeting.txt && cat … && git status … )` →
    `Denied by auto mode classifier ∙ Auto mode could not evaluate this action
    and is blocking it for safety — run with --debug for details`
  - `git config user.name 'Overmind Agent' && git config user.email …` →
    Denied (cùng message); `git add greeting.txt` (đơn lẻ!) → Denied.
  - Xen kẽ nhiều lần lỗi backend classifier: `Error: fable-5 is temporarily
    unavailable, so auto mode cannot determine the safety of Bash right now…`
    (outage làm nhiễu — không tách được phần nào do outage, phần nào do
    classifier "thật"; release notes claude-code cùng ngày còn nhắc bug
    classifier HTTP 401).
  - Tool `Write(greeting.txt)` được auto-approve (đúng spec auto mode);
    `git status --porcelain` đơn lẻ chạy được.
- Kết cục t1: agent dồn về 1 lệnh compound `git add && git -c … commit` → rơi
  xuống prompt hỏi tay → **1 lần bấm Enter**. t2 tương tự → **2 lần bấm**.
  Tổng plan 1: 2 event `task_needs_human`, **3 lần bấm tay**.
- Pipeline sau đó chuẩn: t1 tier-0 pass, system-commit không cần → merge
  52c0243 (commit greeting.txt = 39addb5); t2 (session om-e2e6-1784648067-2)
  commit 9900a8d "Add reply.txt quoting greeting" → merge 093da67 → plan DONE.
  Chaining verify: `git show main:greeting.txt` = `hello-om-e2e6-1784648067`;
  `main:reply.txt` = `Thank you for your kind message
  "hello-om-e2e6-1784648067"; it was a pleasure to receive it.` — TRÍCH đúng.

### Plan 2 — nấc `accept-edits` (p-72724723, so sánh): 4 lần bấm, plan FAILED (bug riêng)
- Cùng repo, goal tương tự (greeting2/reply2), `om run --autonomy=accept-edits`.
- **Mọi bash compound đều hỏi tay**, lý do nguyên văn lần lượt:
  `Contains shell syntax (;) that cannot be statically analyzed`,
  `Parser did not consume trailing input`,
  `This command uses shell operators that require approval for safety`,
  `Contains shell syntax (string) that cannot be statically analyzed`.
  Compound READ-ONLY (`git status; echo; cat | od; git log`) thì chạy không
  hỏi; Write auto-approve. Tổng: 4 `task_needs_human`, **4 lần bấm** trong
  3 round của t1.
- Cả 3 round t1 worker đều làm ĐÚNG việc + marker ok: round 0 (session -3)
  commit d0ed3ce "Add greeting2.txt", round 1 (session -4) commit 878355f,
  round 2 (session -5) commit 61ee168.
- Plan fail KHÔNG phải do autonomy mà do bug mới (dưới): 3 round đều chết cùng
  một check tier-0 → `task_failed {"kind":"verify_budget_exhausted",
  "check_output":"greeting2.txt\n"}` → plan_failed. **Đây là lần đầu nhánh
  budget-exhausted của OM-10 chạy sống** (lần 5 chưa thử được): budget đếm
  đúng 2 retry, round 2 hết budget fail task, plan fail vì "no runnable tasks
  remain" — đúng thiết kế.

### BUG OM THẬT tìm được nhờ E2E — ĐÃ FIX: planner check vs marker protocol
- ROOT CAUSE: planner sinh check tier-0 cho t1 `test "$(cat greeting2.txt)" =
  "…" && git ls-files --error-unmatch greeting2.txt && test -z "$(git status
  --porcelain)"`; nhưng tier-0 verify chạy check bằng `sh -c` NGAY TRONG
  session worktree, nơi OVERMIND PROTOCOL bắt worker để marker
  `.om-done.<hex8>` **untracked** (tạo, không commit) → `git status
  --porcelain` KHÔNG BAO GIỜ sạch → check fail deterministic 100% mọi round,
  dù worker làm đúng cả 3 round, bất kể autonomy.
- System-commit đã exclude marker qua pathspec (`HasUncommitted(…,
  [DoneMarkerPrefix+"*"])` trong poll.go) nhưng tier-0 check KHÔNG có
  mitigation — `runCheck` chạy nguyên văn lệnh planner sinh, planner không
  biết về marker. Plan 1 thoát nạn vì check của nó tình cờ không đòi
  porcelain sạch → bug NON-DETERMINISTIC theo từng plan.
- **Fix (đã validate live rồi implement)**: scheduler ghi pattern `.om-done.*`
  vào `<repo>/.git/info/exclude` lúc run start — ghi ở git common dir nên áp
  dụng cho MỌI linked worktree — qua `gitops.EnsureExcluded`, gọi trong
  `Scheduler.Run` sau `resolveRepo`; kèm dặn planner (prompt) không sinh
  check assert worktree sạch.

### Run 3 — nấc `bypass-permissions` (p-d29c03c4): ZERO-TOUCH PASS
- Chạy ngay sau khi fix EnsureExcluded được build vào /tmp/om và bật
  `autonomy_allow_bypass: true` trong ~/.overmind/config.yaml (opt-in guard).
- Cùng repo test, goal tương tự (greeting3/reply3, 2 task chained),
  `om run --autonomy=bypass-permissions`. Autonomy log:
  `permissions accept-edits → bypass-permissions`.
- **0 lần NEEDS HUMAN trong toàn bộ log, không một prompt nào — plan done
  trong ~90 giây.**
- t1 (session om-e2e6-1784648067-6, round 0): tier-0 verify pass ngay lần
  đầu → merge 512fdce (commit greeting3.txt = 89ec310).
- t2 (session om-e2e6-1784648067-7, round 0): tier-0 pass + tier-1 (LLM
  verify) pass → merge a4f9e30 (commit reply3.txt = 3759e0c), nội dung quote
  nguyên văn `hello3-om-e2e6-1784648067`.
- `git status --porcelain` trên repo chính SẠCH sau run — marker bị exclude
  bởi `.git/info/exclude`, fix EnsureExcluded hoạt động sống.

### So sánh 3 nấc + kết luận cho default
| Nấc | needs_human | Bấm tay | Ghi chú |
|---|---|---|---|
| `auto` (p-e817ba09) | 2 | 3 | Write auto-OK; classifier chặn cả `git add` đơn lẻ; nhiễu bởi outage backend classifier |
| `accept-edits` (p-72724723) | 4 | 4 | Write auto-OK; MỌI bash compound (`&&`, `;`, `$( )`) đều hỏi; read-only compound thì không |
| `bypass-permissions` (p-d29c03c4) | 0 | 0 | ZERO-TOUCH; cần opt-in `autonomy_allow_bypass`; plan done ~90s |
- **Zero-touch chỉ đạt ở `bypass-permissions`**. Acceptance criterion
  "goal A→B chạy 0 lần bấm" của OM-11 PASS về mặt demo với nấc này (cơ chế
  set permission PASS cả 3 nấc).
- Đề xuất: GIỮ default `auto` (ít prompt hơn, đỡ hơn accept-edits về bản chất
  vì classifier còn cải thiện theo model; outage hôm nay là confounder — đáng
  thử lại khi backend ổn). Unattended thật thì dùng `bypass-permissions`
  sau opt-in `autonomy_allow_bypass` + khuyến cáo sandbox.
- Kết luận OM-11c chốt: zero-touch ĐẠT ĐƯỢC với `bypass-permissions` (sau
  opt-in `autonomy_allow_bypass`); `auto` và `accept-edits` KHÔNG zero-touch
  được với Claude Code vì compound bash luôn prompt; bug marker × tier-0 đã
  fix bằng `gitops.EnsureExcluded` (validate sống ở run 3).

### Dọn dẹp
- Cả 3 `om run` tự thoát khi plan xong (done/failed); không còn process om run
  hay tmux session om-e2e6. GIỮ hiện trường:
  repo /tmp/om-e2e6-1784648067, logs /tmp/om-e2e6-run.log /tmp/om-e2e6-run2.log,
  ~/.overmind/overmind.db, ~/.overmind/config.yaml.
