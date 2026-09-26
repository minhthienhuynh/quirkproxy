# Deploy

CI/CD tự động: push `main` → gates Go → build/push image GHCR (`linux/arm64`) → Dokploy redeploy.

## Prerequisites (việc tay, làm một lần)

1. **Dokploy app**: tạo app trong panel Dokploy (đã có: `L3Ppm-oMfUAEODO4Ri6p3`), cấu hình:
   - Image: `ghcr.io/minhthienhuynh/quirkproxy:latest`
   - Port: `8080` (internal, KHÔNG public ra internet — proxy không có auth)
   - Healthcheck: `GET /healthz`
2. **GitHub secrets** (repo → Settings → Secrets → Actions):
   - `DOKPLOY_URL` — panel base URL (VD: `https://dokploy.example.com`, không có `/api` suffix)
   - `DOKPLOY_API_KEY` — API key từ Dokploy
   - `DOKPLOY_APP_ID` — `L3Ppm-oMfUAEODO4Ri6p3`

## Flow

```
push main → .github/workflows/ci.yml
  ├─ gates:   make vet / fmt / test / race  (fail → dừng)
  ├─ build:   docker buildx push ghcr.io/.../quirkproxy:latest + :<sha>
  └─ trigger: POST {DOKPLOY_URL}/api/application.redeploy
```

## Rollback

Dokploy redeploy lại tag sha cũ (image cũ còn trên GHCR).

## Security

- Image bake config `config.docker.json` (listen `0.0.0.0:8080`, `zen_api_key` default `"public"`).
- Không commit key/token vào image. Nếu cần `zen_api_key` riêng, mount file config vào container thay vì bake.
- **KHÔNG publish port ra internet công cộng** — quirkproxy không có auth.
