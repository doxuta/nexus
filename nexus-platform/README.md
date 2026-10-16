# Nexus Platform — Backend (Go)

Metadata-driven core application platform. The backend reads entity/field/relationship/layout
definitions from `sys_*` tables at runtime and exposes CRUD APIs for `obj_*` dynamic data —
no business module is hardcoded in Go. **Dynamic Entity Engine (entity + field + 1-N relation)
is complete (M1+M2+M3, 18/18);** Auth/Security + Layout are next. See `docs/ARCHITECTURE.md`.

## Yêu cầu
- Go **1.22+**
- Docker (MySQL 8 + Redis qua `docker-compose.yml` ở thư mục gốc)
- `golang-migrate` (tùy chọn, để chạy migration đúng chuẩn)

## Chạy nhanh

### macOS / Linux
```bash
# 1) Hạ tầng (từ thư mục gốc repo)
docker compose up -d                       # MySQL 8 + Redis, tự tạo DB nexus_platform

# 2) Backend (từ nexus-platform/)
cp .env.example .env                        # chỉnh nếu cần
go mod download
make migrate-up                             # hoặc: xem "Áp migration không cần golang-migrate" bên dưới
make run                                    # PORT=8099 make run nếu 8080 bận

# 3) Kiểm thử
curl http://127.0.0.1:8080/health
curl http://127.0.0.1:8080/api/v1/metadata/entities
```

### Windows (PowerShell)
```powershell
# 1) Hạ tầng (từ thư mục gốc repo)
docker compose up -d

# 2) Backend (từ nexus-platform\)
Copy-Item .env.example .env
go mod download
go run cmd/server/main.go                   # đặt $env:PORT="8099" nếu 8080 bận

# 3) Kiểm thử
curl.exe http://127.0.0.1:8080/health
curl.exe http://127.0.0.1:8080/api/v1/metadata/entities
```

## Áp migration không cần golang-migrate (dùng Docker)
```bash
# Lưu ý: thêm --default-character-set=utf8mb4 để không hỏng tiếng Việt khi pipe SQL
docker compose exec -T mysql mysql --default-character-set=utf8mb4 nexus_platform < migrations/000001_create_sys_metadata.up.sql
docker compose exec -T mysql mysql --default-character-set=utf8mb4 nexus_platform < migrations/000002_seed_reference_entities.up.sql
```

## Cấu trúc
```
nexus-platform/
├── cmd/server/        # entrypoint
├── internal/
│   ├── handlers/      # API layer (gin)
│   ├── services/      # business logic + orchestration (entity/field/record)
│   ├── metadata/      # metadata repository — sys_* (GORM)
│   ├── dynrepo/       # dynamic obj_* CRUD — sqlx (parameterized)
│   ├── ddl/           # safe SQL builder (sanitize) cho obj_* lúc runtime
│   ├── auth/          # JWT (spec specs/001-auth-security/spec.md — A1–A6 shipped)
│   ├── config/        # config loader
│   └── db/            # DB connection (GORM)
├── migrations/        # SQL migrations (up/down)
└── docs/              # Technical Documents (ARCHITECTURE.md, specs)
```

## Lệnh thường dùng
`make help` để xem tất cả: `build`, `run`, `test`, `vet`, `check`, `migrate-up/down`, `docker-up/down`.
