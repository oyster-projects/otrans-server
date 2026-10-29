# 飞牛 fnOS 部署

## 1. 选择安装包

在 fnOS 开启 SSH 后可执行：

```bash
uname -m
```

- `x86_64` / `amd64`：使用 `dist/fnos-x86/OTrans.fpk`。
- `aarch64` / `arm64`：使用 `dist/fnos-arm/OTrans.fpk`。

不需要在 NAS 上安装 Go、Docker 或 SQLite。Go 服务和 SQLite 驱动已编译到 FPK 的单一 Linux 可执行文件中。

## 2. 手动安装

1. 打开 fnOS 的“应用中心”。
2. 进入“设置”或右下角的“手动安装”。
3. 选择与 NAS CPU 架构对应的 `OTrans.fpk`。
4. 在安装向导中设置 16-128 位管理员令牌，仅允许字母、数字、`_` 和 `-`。建议使用下方命令生成随机令牌并妥善保存。
5. 单文件上传上限可保留默认值 `4294967296`（4 GiB）。
6. 中转文件总配额默认 `107374182400`（100 GiB），达到配额后会拒绝新文件但不会删除现有记录。
7. 安装完成后启动“OTrans 中转站”。

安装完成后，fnOS 桌面和应用中心会出现“OTrans 中转站”入口。点击后即可在管理页输入管理员令牌、生成一次性配对码；手机上传成功后，点击“刷新记录”还能直接查看已提交的文本和文件信息。也可直接访问 `http://<NAS-IP>:18990/admin/`。

可在 Windows PowerShell 生成测试管理员令牌：

```powershell
[Convert]::ToHexString([Security.Cryptography.RandomNumberGenerator]::GetBytes(24))
```

## 3. 检查服务

FPK 当前默认侦听 `18990`：

```powershell
Invoke-RestMethod 'http://<NAS-IP>:18990/healthz'
```

正常结果：

```json
{"status":"ok"}
```

如果无法访问，先检查应用中心中 OTrans 是否正在运行，以及 NAS 防火墙是否允许 TCP `18990`。

## 公网访问安全要求

- 不要把 TCP `18990` 直接转发到公网。
- 外部访问必须使用带有效证书的 HTTPS 反向代理或可信 VPN，并转发 WebSocket Upgrade。
- 反向代理到 OTrans 的上游端口应只允许 NAS 本机或可信局域网访问。
- 如果管理员令牌或设备令牌曾通过公网明文 HTTP 使用，应更换管理员令牌，并在管理页撤销后重新配对相关设备。

## 4. 生成手机配对码

推荐直接打开 `http://<NAS-IP>:18990/admin/`，在页面中点击“生成临时配对码”。下面的 PowerShell 命令仅作为备用方式。

```powershell
$nas = 'http://192.168.1.10:18990'
$adminToken = '安装时设置的管理员令牌'
$headers = @{ 'X-OTrans-Admin-Token' = $adminToken }
Invoke-RestMethod -Method Post -Uri "$nas/api/v1/admin/pair-codes" -Headers $headers
```

返回的 `code` 在 10 分钟内可使用一次。

## 5. 手机端配置

打开 OTrans，在“飞牛连接”区域填写：

- 服务地址：`http://<NAS-IP>:18990`
- 设备名称：例如“我的手机”
- 配对码：上一步返回的 `code`

点击“使用配对码连接”后，服务地址和设备令牌会保存在应用本地。

## 重新打包或换端口

Windows 版 `fnpack` 发布文件可能没有 `.exe` 扩展名，重命名为 `fnpack.exe` 即可：

```powershell
Copy-Item 'D:\download\fnpack-1.2.3-windows-amd64' 'D:\download\fnpack.exe'

# x86 包
.\scripts\build-fpk.ps1 -Platform x86 -ServicePort 18990 -FnpackPath 'D:\download\fnpack.exe'

# ARM64 包
.\scripts\build-fpk.ps1 -Platform arm -ServicePort 18990 -FnpackPath 'D:\download\fnpack.exe'
```

换端口时只需替换 `-ServicePort` 参数，随后用新 FPK 安装，并在各客户端更新服务地址。
