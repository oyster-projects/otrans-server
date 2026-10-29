# OTrans 同步协议 v3

OTrans-FN 是数据权威中心。v2 允许客户端保存同一台服务器的内网、外网两个 HTTP(S) 地址，并始终优先尝试内网。v3 增加实验性局域网文件源发现；客户端通过持久化的 `serverId` 防止两个地址误指向不同服务器。

## 核心对象

- **Device**：一台已配对客户端。每台设备拥有独立令牌、同步模式和事件游标。
- **Transfer**：用户的一次中转操作，可同时包含文本和多个文件。
- **Blob**：按 SHA-256 寻址的文件内容。相同内容只保存一份。
- **Event**：严格递增的变更记录，目前包括 `transfer.created` 和 `transfer.deleted`。

## 配对

1. 管理员使用安装时设置的 `OTRANS_ADMIN_TOKEN` 请求 `POST /api/v1/admin/pair-codes`。
2. 服务返回一个十分钟有效、仅可使用一次的配对码。
3. 手机或 Windows 调用 `POST /api/v1/pair`，获得设备令牌。
4. 后续请求统一使用 `Authorization: Bearer <device-token>`。

设备令牌只在配对成功时返回一次，服务端仅保存其 SHA-256 摘要。管理员可通过管理页撤销设备；撤销后该设备的 HTTP、WebSocket、待使用票据和局域网登记立即失效。

## 上传事务

1. 客户端先计算每个文件的 SHA-256 和字节数。
2. `POST /api/v1/transfers` 创建草稿，并附带当前设备生成的 `clientTransferId`。响应中的 `missingBlobs` 表示服务端尚未拥有的内容。同一设备重复提交相同 `clientTransferId` 时返回原草稿或已提交记录。
3. 对每个缺失内容调用 `PUT /api/v1/blobs/{sha256}`，请求体为原始文件字节。
4. 调用 `POST /api/v1/transfers/{id}/commit`。只有全部 Blob 都存在且长度匹配时才能提交。
5. 提交成功后服务端生成一次 `transfer.created` 事件，其他设备即可拉取；重复提交同一记录不会重复生成事件。

当前 v1 的单 Blob 上传是完整 `PUT`；后续可以增加分块会话而不改变 Transfer 和 Event 模型。下载通过标准 HTTP Range 自动支持断点续传。

## 实时通知与增量同步

Windows 客户端先使用设备令牌调用 `POST /api/v1/ws-ticket`，再用返回的 30 秒一次性票据连接：

```text
GET /api/v1/ws?ticket=<one-time-ticket>
```

WebSocket 只通知 `events.available` 和最新事件序号，不直接承载文件或完整事件。连接建立、断线重连、系统休眠恢复后，客户端都必须继续按持久化游标调用 HTTP 事件接口，因此通知丢失不会造成数据丢失。

服务端以设备 ID 为键只保留一个 WebSocket。相同设备建立新连接时，服务端必须立即关闭此前连接，避免休眠恢复、网络切换或客户端异常退出留下重复连接。公开的 `GET /healthz` 只返回服务状态；实时连接数、协程数和存储用量仅通过管理员接口读取。

客户端调用：

```text
GET /api/v1/events?after=<last-sequence>&limit=100
```

按顺序处理完成后调用：

```text
PUT /api/v1/devices/me/cursor
{"cursor": 123}
```

客户端必须把文件保存到临时文件、校验 SHA-256，再原子移动到 Inbox，避免程序中断后留下半个文件。

## 实验性局域网直传

局域网直传只优化文件下载，不替代飞牛的上传、记录、事件或删除流程：发送方仍先把 Blob 上传飞牛，再将本机已有的摘要和私网 HTTP 地址登记到 `POST /api/v1/lan/announce`。登记有效 90 秒，Windows 客户端每 45 秒续期。

接收方按摘要请求 `GET /api/v1/lan/sources/{sha256}`。飞牛只返回其他设备登记的私网、环回或链路本地 IP 地址，并为每个来源签发 30 秒一次性票据。接收方使用票据向来源设备的 `/v1/blobs/{sha256}` 获取内容；来源设备通过 `POST /api/v1/lan/tickets/redeem` 向飞牛核销票据后才发送文件。

接收方必须重新校验字节数和 SHA-256。节点不可达、票据过期、校验失败或浏览器安全策略阻止访问时，应直接回退 `GET /api/v1/blobs/{sha256}`。Windows 节点启动时由系统动态分配可用端口，再登记包含实际端口的地址；该端口不需要也不应映射到公网，飞牛仍只需映射一个服务端口。

## 删除

`DELETE /api/v1/transfers/{id}` 会：

1. 标记 Transfer 已删除。
2. 生成 `transfer.deleted` 事件。
3. 立即清理没有被其他有效 Transfer 引用的 Blob。
4. 客户端收到事件后，删除自己管理的 Inbox/缓存副本。

用户已经复制或移动到 OTrans 管理目录之外的文件不在同步删除范围内。

## v3 HTTP 端点

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/healthz` | 健康检查 |
| GET | `/api/v1/server-info` | 返回持久化 `serverId` 和协议版本 |
| POST | `/api/v1/admin/pair-codes` | 创建一次性配对码 |
| GET | `/api/v1/admin/status` | 管理员读取连接数和存储用量 |
| GET | `/api/v1/admin/devices` | 管理员查看已配对设备 |
| DELETE | `/api/v1/admin/devices/{id}` | 管理员撤销设备 |
| GET | `/api/v1/admin/transfers` | 管理员查看中转记录 |
| POST | `/api/v1/pair` | 设备配对 |
| GET | `/api/v1/devices/me` | 当前设备信息 |
| PUT | `/api/v1/devices/me/cursor` | 提交同步游标 |
| POST | `/api/v1/transfers` | 创建上传草稿 |
| PUT | `/api/v1/blobs/{sha256}` | 上传文件实体 |
| POST | `/api/v1/transfers/{id}/commit` | 提交中转记录 |
| GET | `/api/v1/transfers` | 查询当前记录 |
| GET | `/api/v1/transfers/{id}` | 查询一条记录 |
| DELETE | `/api/v1/transfers/{id}` | 全局删除 |
| GET | `/api/v1/blobs/{sha256}` | 下载文件实体，支持 Range |
| GET | `/api/v1/events` | 拉取增量事件 |
| POST | `/api/v1/ws-ticket` | 创建 30 秒一次性 WebSocket 票据 |
| GET | `/api/v1/ws` | 建立实时事件通知连接 |
| POST | `/api/v1/lan/announce` | 登记当前设备可提供的局域网地址和 Blob 摘要 |
| GET | `/api/v1/lan/sources/{sha256}` | 查询局域网来源并签发短期一次性票据 |
| POST | `/api/v1/lan/tickets/redeem` | 来源设备核销一次性下载票据 |
