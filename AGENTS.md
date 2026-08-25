# Repository Guidelines

## Project Structure & Module Organization

`cmd/easy_proxies/` contains the Go application entry point. Backend code lives under `internal/`, organized by responsibility: `builder` creates sing-box options, `boxmgr` manages runtime instances, `outbound/pool` implements node selection, `monitor` serves the API and UI, and `store` owns SQLite persistence. Go tests are colocated with their packages as `*_test.go`.

The React 19 and TypeScript frontend is in `frontend/src/`; reusable UI belongs in `components/`, API calls in `api/`, and shared types/utilities in `types/` and `utils/`. Production frontend output is embedded from `internal/monitor/assets/`. Do not hand-edit generated hashed assets.

## Build, Test, and Development Commands

- `go test ./...` runs all backend tests.
- `go build -tags "with_utls with_quic with_grpc with_wireguard with_gvisor" -o easy-proxies ./cmd/easy_proxies` builds the production-capable backend.
- `go run -tags "with_utls with_quic with_grpc with_wireguard with_gvisor" ./cmd/easy_proxies --config ./config.yaml` runs locally after copying `config.example.yaml` to `config.yaml`.
- `cd frontend && npm ci && npm run dev` starts the Vite development server.
- `cd frontend && npm run lint && npm run build` lints, type-checks, and builds the frontend.
- `cd frontend && npx vite build --outDir ../internal/monitor/assets` refreshes assets for an embedded Go build.
- `docker compose up -d --build` builds and starts the complete container stack.

Use Go 1.25 and Node.js 22 to match CI and Docker builds.

## Coding Style & Naming Conventions

Format Go files with `gofmt`; use tabs, short lowercase package names, and `PascalCase` only for exported identifiers. Keep package boundaries under `internal/` focused and avoid bypassing the store or box manager abstractions.

Frontend code uses two-space indentation, `PascalCase` component filenames, and `camelCase` hooks, helpers, and variables. Run ESLint before submitting changes.

## Testing Guidelines

Use Go's standard `testing` package. Name tests `TestBehavior`, prefer table-driven cases for parsers and configuration variants, and keep fixtures deterministic. Add regression coverage near the changed package. There is no fixed coverage threshold, but `go test ./...`, frontend lint, and frontend build should pass before review.

## Commit & Pull Request Guidelines

History follows concise Conventional Commit-style subjects such as `feat(store): ...`, `deps: ...`, and `build: ...`. Keep commits scoped and imperative. PRs should explain the behavior change, configuration or database impact, and exact validation commands. Link relevant issues and include screenshots for visible UI changes. Never commit real subscriptions, credentials, `config.yaml`, SQLite data, or certificates; keep examples sanitized and leave `skip_cert_verify` disabled by default.
