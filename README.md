# GoPrint

*Next-gen SAST Printer Utility*

GoPrint 是一个基于 Golang 的打印后端服务，通过 CUPS/IPP 与系统打印服务通信，并提供 REST API 供前端调用。

## 特性

- 打印机列表与详情查询
- 打印任务提交（multipart 文件上传）
- 手动双面打印（奇偶页拆分 + 二段式提交）
- 打印任务列表、状态查询（支持后台自动刷新到任务存储）
- 删除任务记录（仅删除任务存储中的记录）
- 飞书文档/知识库导出并打印（支持 wiki 节点自动解析、快捷方式跟踪）
- N-up 缩印打印（2-up / 4-up / 6-up），自动计算最优页面布局
- 页面缩放打印（按百分比调整 PDF 页面尺寸）
- 飞书 Bot 交互打印（消息卡片、文件接收、事件解密）
- 状态细化返回（`status`、`reason`、`raw_state`）

## 技术栈

- `gin-gonic/gin`：HTTP API 框架
- `phin1x/go-ipp`：IPP/CUPS 客户端
- `larksuite/oapi-sdk-go/v3`：飞书开放平台 SDK（OAuth 鉴权、文档导出、消息发送、事件订阅）
- `pdfcpu/pdfcpu`：PDF 页面处理（N-up 缩印、页面提取、合并、缩放、旋转）

## 运行要求

- Linux 系统并已安装/启用 CUPS
- Go 1.24+
- 当前进程用户对 CUPS 有查询/提交任务权限
- 已提供并配置 `config.yaml`

## 快速开始

1. 安装依赖

```bash
go mod tidy
```

或使用 Makefile：

```bash
make deps    # 安装依赖
make run     # 启动开发服务器
make build   # 编译到 bin/goprint
make test    # 运行测试
make clean   # 清理构建产物
```

2. 启动服务

```bash
go run main.go
```

服务启动默认读取项目根目录的 `config.yaml`，也支持传入自定义路径：

```bash
go run main.go /path/to/config.yaml
```

3. 服务地址

- 默认端口：`5001`
- Base URL：`http://localhost:5001`

## Docker 分离部署（推荐）

为减小主服务镜像体积，Office 转换服务已支持独立镜像部署。

- `goprint`：Go 后端 API（5001）
- `office-converter`：Python gRPC 转换服务（50061，容器内通信）

两者通过 Docker 网络通信，地址由 `office_conversion.grpc_address` 指向 `office-converter:50061`。
同时通过共享卷 `office-output` 交换转换后的 PDF 文件。

1. 准备配置

编辑项目根目录的 `config.yaml`，并确保：

- `office_conversion.enabled: true`
- `office_conversion.start_with_server: false`
- `office_conversion.grpc_address: office-converter:50061`
- `office_conversion.output_dir: /tmp/office-output`

2. 启动

```bash
docker compose up -d --build
```

3. 查看日志

```bash
docker compose logs -f goprint office-converter
```

4. 字体与 WPS 配置

当前 `office-converter` 镜像构建流程已自动注入字体与 `Office.conf`。请将需要的字体放入 `office_converter/assets/fonts` 下，可用的配置文件放入 `office_converter/assets/Office.conf`。由于文件太大，仓库无法提供这些文件。

如需排查转换环境，可执行以下可选检查命令：

```bash
docker exec sast-office-converter sh -lc "fc-list | wc -l"
```

## API 概览

### 健康检查

- `GET /health`

### 鉴权接口

- `GET /api/auth/config`：返回飞书 OAuth 配置（`app_id` 等）
- `GET /api/auth/config/authorize-url`：生成飞书 OAuth 授权地址
- `POST /api/auth/config/code-login`：用飞书 code 换取 token 和用户信息
- `GET /api/auth/config/jssdk-config`：获取飞书 H5 JSSDK 鉴权签名

### 打印机接口

- `GET /api/printers`：获取打印机列表
- `GET /api/printers/:id`：获取打印机详情；如该打印机存在活跃任务，响应可能包含 `active_job_warning`

### 任务接口

- `POST /api/jobs`：提交打印任务
- `POST /api/jobs/preview`：转换文件并返回预览 PDF
- `GET /api/jobs/supported-file-types`：获取当前支持的文件类型列表（PDF、配置的 Office 格式、jpg/jpeg/png）
- `GET /api/jobs`：获取任务列表
- `GET /api/jobs/:id`：获取任务详情/状态
- `DELETE /api/jobs/:id`：删除任务记录（不会向 CUPS 下发取消）

### 飞书文档接口

- `POST /api/jobs/preview/feishu`：导出飞书文档/知识库页面为 PDF 并返回预览
- `POST /api/jobs/feishu`：导出飞书文档/知识库页面为 PDF 并提交打印

### 手动双面 Hook 接口

- `POST /api/manual-duplex-hooks/:token/continue`：提交手动双面剩余页面
- `POST /api/manual-duplex-hooks/:token/extend`：在允许延时窗口内延长手动双面等待时间
- `POST /api/manual-duplex-hooks/:token/cancel`：取消手动双面并清理暂存文件

### 飞书 Bot 接口

- `POST /api/bot/events`：接收飞书事件订阅推送（消息接收、卡片回调、URL 验证）

Bot 完整交互流程参见下方「飞书 Bot 使用说明」。

### scanservjs 代理

- `ANY /sane-api/*`：反向代理到 `sane_api.target_url`（默认 `http://192.168.101.37:8080`）
- 代理会透传方法、查询参数、请求体和响应体（适用于 scanservjs 全部接口）
- 路径映射规则：`/sane-api/<path>` -> `<target>/<path>`
- 对上游返回的重定向 `Location` 会自动补全 `/sane-api` 前缀，避免跳回本地前端路由
- 鉴权策略：
    - `sane_api.auth_enabled: false` 时，直接代理（适用于可信内网）
    - `sane_api.auth_enabled: true` 时，鉴权失败直接拒绝（`401`）
    - 优先校验 `sane_api.auth_header` / `Authorization: Bearer` 是否匹配 `sane_api.auth_token`
    - 若未配置 `sane_api.auth_token` 且 `auth.enabled: true`，则走全局飞书 Bearer 鉴权

## 飞书 Bot 使用说明

飞书 Bot 支持用户在群聊（@Bot）或私聊中发送文件/云文档链接，通过消息卡片配置打印参数后提交打印。

### 启用 Bot

1. 在飞书开放平台 → 应用 → 添加「机器人」和「事件订阅」能力
2. 事件订阅配置：
   - 请求网址：`https://<你的域名>/api/bot/events`
   - 订阅事件：`im.message.receive_v1`、`card.action.trigger`
3. 在 `config.yaml` 中配置：

```yaml
bot:
  enabled: true
  verification_token: "与飞书后台一致"
  encrypt_key: ""                         # 若开启事件加密则填写
  bot_name: "GoPrint"
  card_timeout: 10m
```

### 交互流程

```
用户 @Bot 发送文件 / 链接
       ↓
Bot 回复参数配置卡片（打印机、份数、页码范围、缩放、缩印、单双面）
       ↓
用户修改参数 → 点击「开始打印」
       ↓
如所选打印机存在活跃任务，Bot 提示确认是否仍然打印
       ↓
Bot 提交打印任务 → 保存记录到多维表格
```

手动双面时，第一面打印完成后 Bot 会推送「翻面继续」卡片。

### 事件解密

飞书支持 AES-256-CBC 加密事件推送。配置 `bot.encrypt_key` 后，系统自动解密。解密算法：

- 密钥：`SHA256(encrypt_key)`
- 模式：AES-256-CBC，IV 为密文前 16 字节
- 填充：PKCS7

### 所需权限

| 权限 | 用途 |
|------|------|
| `im:message` | 发送消息/卡片 |
| `im:message:read` | 接收用户消息 |
| `im:resource` | 下载用户发送的文件 |
| `drive:export` | 导出飞书文档为 PDF |
| `bitable:app` | 读写打印记录到多维表格 |

### 默认打印参数

通过 `file_type_defaults` 按文件扩展名配置 Bot 场景下的默认打印参数：

```yaml
file_type_defaults:
  pdf:
    copies: 1
    duplex: auto
    nup: 1
    scale: 100
    collate: true
  doc:
    copies: 1
    duplex: "off"
    nup: 2
    scale: 100
    collate: true
  docx:
    $ref: doc     # 引用 doc 的配置
  _cloud_doc:      # 飞书云文档专用，Bot 识别到链接时自动应用
    copies: 1
    duplex: "off"
    nup: 2
    scale: 100
    collate: true
    direction: horizontal
```

## 示例请求

### 1) 打印机列表

```bash
curl -sS http://localhost:5001/api/printers
```

### 2) 提交打印任务

```bash
curl -sS -X POST http://localhost:5001/api/jobs \
    -F printer_id=sast-color-printer \
    -F file=@printer_test.pdf
```

可选 URL 参数：

- `duplex=true|false`：是否启用双面打印（默认 `false`）
- `copies=整数`：打印份数（默认 `1`）
- `collate=true|false`：份数排列方式（默认 `true`）
- `nup=1|2|4|6`：每版打印页数/缩印（默认 `1`，即不缩印）
- `scale=10-400`：页面缩放比例，单位为百分比整数（默认 `100`）
- `pages=页码范围`：指定打印页，如 `"1-5,10"`（默认全部）

示例（双面 + 2-up 缩印 + 90% 页面缩放）：

```bash
curl -sS -X POST "http://localhost:5001/api/jobs?duplex=true&nup=2&scale=90" \
    -F printer_id=sast-color-printer \
    -F file=@printer_test.pdf
```

### 2.1) 提交手动双面任务

```bash
curl -sS -X POST "http://localhost:5001/api/jobs?copies=1" \
    -F printer_id=sast-color-printer \
    -F file=@printer_test.pdf
```

若该打印机在 `config.yaml` 中配置 `duplex_mode: manual`，响应会返回 `hook_url`，用于第二轮打印。

### 2.2) 触发手动双面第二轮

```bash
curl -sS -X POST http://localhost:5001/api/manual-duplex-hooks/<token>/continue
```

### 3) 查询任务状态

```bash
curl -sS http://localhost:5001/api/jobs/29
```

### 4) 删除任务记录

```bash
curl -sS -X DELETE http://localhost:5001/api/jobs/29
```

说明：该接口仅删除任务存储中的记录，不会取消打印机上的物理任务。

### 5) 获取 JSSDK 鉴权配置

```bash
curl -sS "http://localhost:5001/api/auth/config/jssdk-config?url=https://your-domain.com/printers?id=xxx"
```

返回示例：

```json
{
  "appId": "cli_xxxxxxxxxxxx",
  "timestamp": "1746000000123",
  "nonceStr": "a1b2c3d4e5f6g7h8",
  "signature": "abcd1234efgh5678..."
}
```

前端使用返回值调用 `h5sdk.config()` 完成 JSSDK 鉴权后即可使用 `tt.docsPicker()` 等需鉴权的 JSAPI。鉴权通过 `h5sdk` 命名空间完成，JSAPI 通过 `tt` 命名空间调用，两者独立。

签名算法：`SHA1("jsapi_ticket={ticket}&noncestr={nonceStr}&timestamp={timestamp}&url={pageURL}")`，其中 `timestamp` 为毫秒级 Unix 时间戳，`jsapi_ticket` 通过飞书 Open API `/open-apis/jssdk/ticket/get` 获取。

### 6) 访问 scanservjs

```bash
curl -sS -H 'X-Sane-Api-Key: change_me' http://localhost:5001/sane-api/api-docs
```

### 7) 导出飞书文档预览

```bash
curl -sS -X POST http://localhost:5001/api/jobs/preview/feishu \
    -H 'Authorization: Bearer <user_access_token>' \
    -H 'Content-Type: application/json' \
    -d '{"url":"https://sast.feishu.cn/docx/doxcnXXXXXXXXXXXX"}' \
    -o preview.pdf
```

### 8) 导出飞书文档并打印

```bash
curl -sS -X POST http://localhost:5001/api/jobs/feishu \
    -H 'Authorization: Bearer <user_access_token>' \
    -H 'Content-Type: application/json' \
    -d '{
        "url": "https://sast.feishu.cn/wiki/wikcnXXXXXXXXXXXX",
        "printer_id": "sast-printer",
        "copies": 1,
        "duplex": false
    }'
```

可选 JSON 参数：

- `copies`：打印份数（默认 `1`）
- `duplex`：是否启用双面打印（默认 `false`）
- `collate`：份数排列方式（默认 `true`）
- `nup`：每版打印页数（`1`/`2`/`4`/`6`，默认 `1`）
- `scale`：页面缩放比例，单位为百分比整数，范围 `10`-`400`（默认 `100`）
- `pages`：页码范围（如 `"1-5,10"`）

知识库文档打印示例：

```bash
curl -sS -X POST http://localhost:5001/api/jobs/feishu \
    -H 'Authorization: Bearer <user_access_token>' \
    -H 'Content-Type: application/json' \
    -d '{
        "url": "https://sast.feishu.cn/wiki/wikcnXXXXXXXXXXXX",
        "printer_id": "sast-printer"
    }'
```

飞书文档导出前提条件：

- 自建应用需拥有以下权限：`docs:doc:readonly`、`docx:document:readonly`、`drive:drive:readonly`、`wiki:wiki:readonly`
- 导出知识库文档时，应用须被添加为知识库管理员
- 应用需对目标文档有读权限
- 导出后的文件仅保留 10 分钟，系统会立即下载

等价于直接访问：`http://192.168.101.37:8080/api-docs`。

如果你使用全局飞书鉴权，也可以在启用 `auth.enabled: true` 后，使用 `Authorization: Bearer ...` 调用该路由。

## 任务状态字段

- `status`：归一化后的任务状态（如 `pending`、`processing`、`completed`、`cancelled`）
- `reason`：CUPS/IPP 原始原因（`job-state-reason`）
- `raw_state`：IPP 原始状态码

启用 `job_store.enabled` 时，后台会定期同步 CUPS 任务状态到飞书多维表。除此之外，系统每隔 `47m17s` 会查询一次多维表中仍为 `pending` / `pending_manual_continue` 的异常记录；如果提交时间早于 12 小时前，会将其手动标记为 `completed`，用于清理已经无法从 CUPS 侧可靠确认的历史任务。

## 手动双面说明

- 启用方式：为打印机配置 `duplex_mode: manual`
- `duplex_mode: off`：关闭双面功能，按单轮正常打印
- `duplex_mode: auto`：使用打印机原生双面，按文档方向自动选择：纵向=`two-sided-long-edge`，横向=`two-sided-short-edge`
- `duplex_mode: manual`：执行手动双面（双轮提交 + hook）
- 当请求 `duplex` 未传或为 `false` 时，默认单面打印 1 份
- 当原始文件为 1 页时：无论配置为何，均按单轮打印（不启用双面）
- `reverse` 仅在单面打印时生效；双面打印不遵守此设定
- `first_pass` 可选 `even` 或 `odd`
- `reverse_first_pass` / `reverse_second_pass` 控制两轮页序是否反转
- `rotate_second_pass` 控制二轮文件是否旋转 180 度
- `pad_to_even` 控制奇数页时是否自动补空白页到偶数
- 二轮行为：访问返回的 `hook_url`，系统提交剩余页（奇数页）
- Hook 过期时间按打印页数动态计算：`max(printing.manual_duplex_min_timeout, 打印页数 * printers[].manual_duplex_per_page_timeout)`
- 当剩余时间小于等于 `printing.manual_duplex_extend_window` 时，可调用 `POST /api/manual-duplex-hooks/:token/extend` 延时；成功后过期时间重置为当前时间 + `printing.manual_duplex_min_timeout`
- Hook 接口不走飞书鉴权，令牌本身用于定位待继续的任务
- Hook 是一次性的，成功触发后将失效

## 配置文件

系统使用 YAML 配置文件，不再通过环境变量配置服务参数。

默认文件：`config.yaml`

示例：

```yaml
server:
    host: 0.0.0.0
    port: 5001

auth:
    enabled: true
    session:
        secret: "至少 32 字节的高熵随机字符串"
        secure: true
        max_age_seconds: 604800
    feishu:
        app_id: cli_xxxxxxxxxxxx
        app_secret: your_app_secret
        redirect_uri: https://your-domain.com/

printing:
    ipp_username: goprint
    queue_wait_timeout: 60s
    max_upload_bytes: 52428800
    max_copies: 100
    max_pdf_pages: 500
    max_image_pixels: 50000000
    manual_duplex_min_timeout: 10m
    manual_duplex_extend_window: 3m
    temp_dir: /tmp/goprint

sane_api:
    target_url: http://192.168.101.37:8080
    auth_enabled: true
    auth_header: X-Sane-Api-Key
    auth_token: change_me

job_store:
    enabled: false
    feishu:
        app_token: bascnxxxxxxxxxxxx
        table_id: tblxxxxxxxxxxxx
        request_timeout: 3s

office_conversion:
    enabled: true
    start_with_server: false
    grpc_address: 127.0.0.1:50061
    service_script: office_converter/run.sh
    accepted_formats:
        - doc
        - docx
        - ppt
        - pptx
    request_timeout: 60s
    output_dir: /tmp/office-output

bot:
    enabled: false
    verification_token: your_verification_token
    encrypt_key: your_encrypt_key
    bot_name: GoPrint
    card_timeout: 10m

file_type_defaults:
    pdf:
        copies: 1
        duplex: auto
        nup: 1
        scale: 100
        collate: true
        direction: horizontal
    doc:
        copies: 1
        duplex: "off"
        nup: 2
        scale: 100
        collate: true
    jpg:
        $ref: pdf
    _cloud_doc:
        copies: 1
        duplex: "off"
        nup: 2
        scale: 100
        collate: true
        direction: horizontal

printers:
  - id: sast-printer
    uri: ipp://localhost:631/printers/sast-printer
    visible: true
    reverse: false
    duplex_mode: off
    first_pass: even
    pad_to_even: true
    reverse_first_pass: false
    reverse_second_pass: false
    rotate_second_pass: false
    manual_duplex_per_page_timeout: 30s
    note: ""
```

字段说明：

### Server

- `server.host`：监听地址（默认 `0.0.0.0`）
- `server.port`：监听端口（默认 `5001`）

### Auth

- `auth.enabled`：是否启用飞书 OAuth 鉴权（默认 `false`）
- `auth.session.secret`：启用鉴权时必填，至少 32 字节；服务会用它派生签名和加密 Cookie 密钥，可用 `openssl rand -hex 32` 生成
- `auth.session.secure`：是否给会话 Cookie 添加 `Secure` 属性；HTTPS 生产部署应设置为 `true`，本地 HTTP 调试可显式设为 `false`
- `auth.session.max_age_seconds`：会话 Cookie 有效期，默认 `604800`（7 天）
- `auth.feishu.app_id`：飞书自建应用的 App ID
- `auth.feishu.app_secret`：飞书自建应用的 App Secret
- `auth.feishu.redirect_uri`：OAuth 回调地址
- `auth.feishu.authorize_url`：授权页面地址（默认飞书官方地址）
- `auth.feishu.token_url`：Token 交换地址（默认飞书官方地址）
- `auth.feishu.user_info_url`：用户信息地址（默认飞书官方地址）
- `auth.feishu.request_timeout`：飞书 API 请求超时（默认 `3s`）
- `auth.feishu.token_cache_ttl`：Token 缓存有效期（默认 `2m`）

### Printing

- `printing.ipp_username`：IPP 请求用户名（默认 `goprint`）
- `printing.queue_wait_timeout`：打印/预览请求等待全局提交队列的最长时间（默认 `60s`）
- `printing.max_upload_bytes`：上传/导出文件大小限制，单位字节（默认 `52428800`，即 50 MiB）
- `printing.max_copies`：允许的最大打印份数（默认 `100`）
- `printing.max_pdf_pages`：允许处理的最大 PDF 页数（默认 `500`）
- `printing.max_image_pixels`：jpg/png 转 PDF 前允许解码的最大像素数（默认 `50000000`）
- `printing.manual_duplex_min_timeout`：手动双面 hook 最短等待时间（默认 `10m`）
- `printing.manual_duplex_extend_window`：允许用户延长手动双面等待时间的剩余时间窗口（默认 `3m`）
- `printing.manual_duplex_hook_ttl`：兼容旧配置；未设置 `manual_duplex_min_timeout` 时作为最短等待时间回退值
- `printing.temp_dir`：短期上传、预览、导出、手动双面等临时文件目录（默认 `/tmp/goprint`）

### Sane API

- `sane_api.target_url`：scanservjs 后端地址（默认 `http://192.168.101.37:8080`）
- `sane_api.auth_enabled`：是否启用 `/sane-api` 鉴权（默认 `true`）
- `sane_api.auth_header`：共享密钥请求头名称（默认 `X-Sane-Api-Key`）
- `sane_api.auth_token`：共享密钥；当 `auth_enabled: true` 时建议配置

### Job Store

- `job_store.enabled`：是否启用飞书多维表任务存储（默认 `false`）
- `job_store.feishu.app_token`：飞书多维表的 App Token（`bascn...`）
- `job_store.feishu.table_id`：多维表 ID（`tbl...`）
- `job_store.feishu.request_timeout`：请求超时（默认 `3s`）

任务表需要包含 `job_id`、`printer_id`、`file_name`、`status`、`copies`、`page_count`、`duplex`、`duplex_hook`、`duplex_expire_at`、`user`、`submitted_at` 等字段；`duplex_expire_at` 为日期时间字段，格式与 `submitted_at` 一致，用于判断手动双面翻面等待是否仍有效。

### Office Conversion

- `office_conversion.enabled`：是否启用 Office 转 PDF（默认 `false`）
- `office_conversion.start_with_server`：是否随主服务启动转换服务（默认 `false`）
- `office_conversion.grpc_address`：gRPC 转换服务地址（默认 `127.0.0.1:50061`）
- `office_conversion.service_script`：转换服务启动脚本（默认 `office_converter/run.sh`）
- `office_conversion.accepted_formats`：支持的 Office 文件扩展名列表
- `office_conversion.request_timeout`：转换请求超时（默认 `60s`）
- `office_conversion.output_dir`：转换输出目录（默认 `/tmp/office-output`）

### Bot

飞书 Bot 配置，用于接收用户消息并通过卡片交互设置打印参数。

- `bot.enabled`：是否启用飞书 Bot（默认 `false`）
- `bot.verification_token`：飞书事件订阅的 Verification Token
- `bot.encrypt_key`：飞书事件订阅的 Encrypt Key
- `bot.bot_name`：Bot 显示名称（默认 `GoPrint`）
- `bot.card_timeout`：消息卡片超时时间（默认 `10m`）

### File Type Defaults

按文件扩展名配置默认打印参数。支持 `$ref` 引用其他扩展名的配置，避免重复。

- `file_type_defaults.<ext>.copies`：默认打印份数
- `file_type_defaults.<ext>.duplex`：双面模式（`off` / `auto` / `manual`）
- `file_type_defaults.<ext>.nup`：每版打印页数（`1` / `2` / `4` / `6`）
- `file_type_defaults.<ext>.scale`：页面缩放比例，单位为百分比整数（默认 `100`）
- `file_type_defaults.<ext>.collate`：逐份打印（默认 `true`）
- `file_type_defaults.<ext>.direction`：N-up 排版方向（`horizontal` / `vertical`）
- `file_type_defaults.<ext>.$ref`：引用另一扩展名的配置（如 `jpg.$ref: pdf`）
- `file_type_defaults._cloud_doc`：飞书云文档专用默认参数，区别于普通 PDF 文件；Bot 识别到云文档链接时自动应用

### Printers

- `printers[].uri`：按打印机配置完整 URI（支持不同打印机在不同 CUPS 地址）
- `printers[].visible`：是否在 `GET /api/printers` 中返回该打印机
- `printers[].reverse`：单面打印时是否反向页序（双面模式忽略此字段）
- `printers[].duplex_mode`：`off` / `auto` / `manual`（默认 `off`）
- `printers[].first_pass`：`even` / `odd`（默认 `even`）
- `printers[].pad_to_even`：奇数页时是否补空白页（默认 `true`）
- `printers[].reverse_first_pass`：首轮页序反转（默认 `false`）
- `printers[].reverse_second_pass`：二轮页序反转（默认 `false`）
- `printers[].rotate_second_pass`：二轮旋转 180 度（默认 `false`）
- `printers[].manual_duplex_per_page_timeout`：手动双面按页增加的等待时间（默认 `30s`）
- `printers[].note`：该打印机的说明文字
