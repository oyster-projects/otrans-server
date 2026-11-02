# Docker 部署

OTrans Docker 镜像支持 `linux/amd64` 和 `linux/arm64`。容器使用非 root 用户运行，数据只写入 `/data`，API、管理页、网页版和 WebSocket 共用 `8080` 端口。

## 使用 Compose

```bash
cp .env.example .env
# 编辑 .env，生成并填写自己的管理员令牌
docker compose up -d --build
```

访问 `http://NAS地址:8080/admin/` 生成配对码。公网访问应由 Caddy、Nginx、Traefik 或 NAS 自带反向代理提供 HTTPS，并正确转发 WebSocket；不要把容器的原始 HTTP 端口直接暴露到公网。

升级前请保留 `otrans-data` 卷。停止或重建容器不会清除中转数据；执行 `docker compose down -v` 会删除该卷，请勿在需要保留数据时使用。

## 配置项

| 环境变量 | 默认值 | 说明 |
| --- | ---: | --- |
| `OTRANS_ADMIN_TOKEN` | 无 | 必填，至少 16 位 |
| `OTRANS_MAX_UPLOAD_BYTES` | `4294967296` | 单文件上传上限 |
| `OTRANS_MAX_STORAGE_BYTES` | `107374182400` | Blob 总配额，不能小于单文件上限 |
| `OTRANS_RETENTION_DAYS` | `30` | 已提交内容保留天数，`0` 关闭自动删除 |

自动删除每小时检查一次。过期记录会生成正常的全局删除事件，因此 HarmonyOS、Windows、网页端和其他电脑都会同步看到删除；没有其他记录引用的文件实体随后从磁盘回收。

## 构建多架构镜像

```bash
docker buildx build --platform linux/amd64,linux/arm64 -t your-namespace/otrans-server:0.5.3 .
```

发布到 Docker Hub 时需要使用自己账号或组织的命名空间，例如 `your-namespace/otrans-server`。普通公开仓库即可，不需要申请 Docker Official Image。

仓库内的 `docker-publish.yml` 已准备好自动发布流程。创建 Docker Hub 仓库后，在 GitHub 仓库设置中添加：

- Actions 变量 `DOCKERHUB_NAMESPACE`：Docker Hub 用户名或组织名
- Actions 变量 `DOCKERHUB_USERNAME`：用于登录的 Docker ID
- Actions secret `DOCKERHUB_TOKEN`：具有该仓库写权限的 Docker Hub access token

之后推送形如 `v0.5.3` 的 Git tag，会自动发布 `0.5.3`、`0.5`、`0` 和 `latest` 多架构标签，并附带 SBOM 与构建来源证明。
