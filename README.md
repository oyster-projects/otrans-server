# OTrans-FN

OTrans 的飞牛 fnOS 中心服务。它保存中转记录和文件实体，并向 HarmonyOS 26 与 Windows 客户端提供统一同步协议。

## 当前能力

- 一次性配对码和独立设备令牌
- 飞牛管理页可查看已提交的文本、文件信息和来源设备
- 文本、图片及普通文件组成的一批中转记录
- SHA-256 内容寻址与文件去重
- 增量事件游标
- 持久化服务器身份，供客户端校验内外网地址是否指向同一台飞牛
- WebSocket 实时事件通知与一次性短期连接票据
- 客户端上传幂等键，网络切换或超时重试不会重复创建记录
- 全局删除和无引用文件回收
- HTTP Range 下载
- SQLite 单机存储，适合家庭 NAS
- 同端口网页版，支持配对、文本与文件传送、下载校验及全局删除
- v0.5 实验性局域网文件源发现与 30 秒一次性票据；接收端直连失败时继续从飞牛下载

## 本地启动

需要 Go 1.24 或更新版本。

```powershell
$env:OTRANS_ADMIN_TOKEN = 'change-this-development-token'
go run ./cmd/otransd
```

默认监听 `:8080`，数据写入 `./data`。生产环境应由飞牛反向代理或用户自己的入口提供 HTTPS；OTrans 本身不负责内网穿透。

启动后可访问：

- 管理页：`http://飞牛地址:端口/admin/`
- 网页版：`http://飞牛地址:端口/web/`

网页版与 API、WebSocket 共用同一个服务端口，不需要额外映射端口。首次使用时在管理页生成一次性配对码，再在网页版完成配对。浏览器令牌只保存在当前浏览器的本地或会话存储中。

局域网直传也不会增加飞牛的映射端口：飞牛仍只使用一个服务端口。开启实验功能的 Windows 客户端由系统动态分配本机端口，再把实际局域网地址登记给飞牛；该端口仅供同网设备直接取文件，不应映射到公网。文件和记录仍以飞牛为权威副本，局域网节点不可用时自动回退飞牛。

## 获取第一个设备令牌

```powershell
$headers = @{ 'X-OTrans-Admin-Token' = 'change-this-development-token' }
$pair = Invoke-RestMethod -Method Post -Uri 'http://127.0.0.1:8080/api/v1/admin/pair-codes' -Headers $headers
$body = @{ code = $pair.code; name = 'My PC'; platform = 'windows' } | ConvertTo-Json
Invoke-RestMethod -Method Post -Uri 'http://127.0.0.1:8080/api/v1/pair' -ContentType 'application/json' -Body $body
```

完整同步流程见 [docs/protocol.md](docs/protocol.md)。v0.3 及更新客户端需要先通过 `GET /api/v1/server-info` 确认服务器身份；v0.5 的局域网实验功能要求协议版本 3。

## FPK 打包

FPK 是 fnOS 应用中心识别和安装的应用包，不限定内部实现。本项目内部是原生 Linux Go 服务，通过 fnOS 的 FPK 生命周期脚本运行。

```powershell
# 只生成可检查的飞牛目录，不调用 fnpack
.\scripts\build-fpk.ps1 -Platform x86 -SkipFnpack
.\scripts\build-fpk.ps1 -Platform arm -SkipFnpack
```

如果下载的 Windows 版 `fnpack` 没有 `.exe` 扩展名，直接将它重命名为 `fnpack.exe`即可。也可以不修改 `PATH`，直接指定位置：

```powershell
.\scripts\build-fpk.ps1 -Platform x86 -ServicePort 18990 -FnpackPath 'D:\download\fnpack.exe'
.\scripts\build-fpk.ps1 -Platform arm -ServicePort 18990 -FnpackPath 'D:\download\fnpack.exe'
```

FPK 的默认服务端口已改为 `18990`，也可用 `-ServicePort` 指定 1024-65535 之间的其他端口。由于 FPK 内含架构二进制，x86 和 ARM 需要分别出包。
