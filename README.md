# OmniMintX

Repo phát triển **Overmind** — coordinator chạy trên AO daemon (agent-orchestrator): nhận 1 goal, sinh plan dạng DAG, rồi tự động dispatch/giám sát các session agent theo thứ tự dependency. Code sản phẩm nằm trong `brain/` (Go, CLI `om`).

- Tổng kết tiến độ + kế hoạch: [docs/OVERMIND.md](docs/OVERMIND.md)
- Nhật ký E2E chi tiết: [brain/docs/e2e.md](brain/docs/e2e.md)
