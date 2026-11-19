# PicBox 原生二进制安装说明

本压缩包由 `scripts/build-release.sh` 生成，包含：

| 文件 | 说明 |
| --- | --- |
| `picbox` | 静态链接的 Linux 二进制（`amd64` 或 `arm64`，与包名一致） |
| `picbox.service` | 对应架构通用的 systemd 模板，以专用 `picbox` 用户运行 |
| `INSTALL.md` | 本说明 |
| `LICENSE` | PicBox 项目许可证 |
| `THIRD_PARTY_NOTICES.md` | 第三方依赖的许可证与声明 |

`SHA256SUMS` 与压缩包放在同一目录。运行机只需要对应平台的二进制，
**不需要 Go、Node、SQLite 服务或图片处理库**，默认也不使用 Docker。界面是内嵌的静态 React
包，运行机不需要 Node 或浏览器构建步骤；完整可配项见仓库内 `.env.example`。

## 1. 校验与解包

在保存压缩包的目录执行（`SHA256SUMS` 就在旁边）：

```sh
sha256sum -c SHA256SUMS
tar -xzf picbox_<version>_linux_amd64.tar.gz   # arm64 机器换成对应文件名
```

校验必须全部 `OK` 再继续。版本号与架构在包名与 `picbox --version` 的输出中可见：

```sh
./picbox_<version>_linux_amd64/picbox --version
```

`--version` 打印 `version` / `commit` / `buildTime` / `go` / `platform`，不读取配置、不创建数据目录。
输出中出现 `note=本地开发构建` 说明该二进制不是发布构建；`commit=<sha>-dirty` 说明构建时
本仓库源码含未提交或未跟踪的改动（整目录未入 Git 时同样算）——这两种产物与 `HEAD`
不一致，不要在正式环境部署。

## 2. 安装前检查（只读，不写任何系统路径）

先确认这台机器上还没有同名对象。下面每条命令都只读取状态，不创建、不覆盖任何文件。
**先检查、后动手：检查没有全部通过之前，不要执行第 3 节的任何写入命令。**

```sh
# 2.1 专用账号是否已存在（不存在时 id 返回非零，属预期）
id picbox

# 2.2 持久化数据目录是否已存在（模板默认 /var/lib/picbox；不存在时 ls 报 No such file）
ls -ld /var/lib/picbox

# 2.3 目标二进制是否已存在（不存在时报 No such file）
ls -l /usr/local/bin/picbox

# 2.4 服务单元是否已存在（不存在时 systemctl cat 报 could not be found；unit 文件与已加载单元都要看）
ls -l /etc/systemd/system/picbox.service
systemctl cat picbox
```

判定规则（**“对象不存在”导致的非零退出是预期结果，不算检查失败**）：

- `id picbox`、`ls -ld`、`ls -l` 与 `systemctl cat` 在对象不存在时本来就会非零退出
  （`no such user`、`No such file or directory`、`Unit picbox.service could not be found`）。
  这类输出正是首次安装想要的结论，属于**预期结果**，继续看下一项即可。
- 任一条显示对象**已存在**（有账号、有数据目录、有 `/usr/local/bin/picbox`、有同名 unit 文件或
  已加载单元），**立即停止首次安装**，改按第 4 节升级流程处理：新包不能覆盖旧二进制、旧 unit
  或旧数据目录。
- 只有**权限不足**（`Permission denied`）、**命令不存在**（`command not found`）、
  `systemctl` 连不上 systemd 等工具/环境错误，或输出无法判断“存在或不存在”时，才必须停下查清原因；
  这类结果不能当成“对象不存在”继续往下走。
- 四条检查都给出明确结论、且对象都不存在后，才进入第 3 节。

## 3. 首次安装（检查全部通过后才写系统路径）

下方列出手工安装的每一步；构建脚本不会执行 `sudo`、`systemctl` 或远程发布动作。
如需自动完成校验、建号与 systemd 配置，也可使用仓库内的 [`scripts/install.sh`](../scripts/install.sh)
（升级时传 `--upgrade`）。

```sh
# 3.1 专用账号与持久化目录（模板默认使用 /var/lib/picbox）
sudo useradd --system --home-dir /var/lib/picbox --shell /usr/sbin/nologin picbox
sudo install -d -m 0700 -o picbox -g picbox /var/lib/picbox

# 3.2 二进制：先落到临时名并核对版本，再原子替换目标路径
sudo install -m 0555 picbox_<version>_linux_amd64/picbox /usr/local/bin/.picbox.new
/usr/local/bin/.picbox.new --version   # 版本/架构必须与包名一致；出现 note= 或 -dirty 不得部署
sudo mv /usr/local/bin/.picbox.new /usr/local/bin/picbox

# 3.3 服务单元：先看模板内容，再安装
cat picbox_<version>_linux_amd64/picbox.service   # 确认 User/WorkingDirectory/监听地址符合本机
sudo install -m 0644 picbox_<version>_linux_amd64/picbox.service /etc/systemd/system/picbox.service
sudo systemctl cat picbox   # 安装后复核：生效的是刚写入的 unit
```

服务模板的关键约定：

- `User=picbox` / `StateDirectory=picbox` / `PICBOX_DATA_DIR=/var/lib/picbox`：
  `data/` 是整体持久化单元，里面有 SQLite 数据库、`auth.key` 与本地上传的图片。
- `EnvironmentFile=-/etc/picbox.env`：可选，用于设置 `PICBOX_ADDR`、`BASEURL`、
  `UPLOAD_MAX_BYTES`、`USER_QUOTA_BYTES` 等；文件不存在时不报错。
- `Environment=TRUSTED_PROXIES=127.0.0.1/32,::1/128`：只信任本机环回反代。
  未设置 `TRUSTED_PROXIES` 时程序忽略所有转发头（`X-Forwarded-For` / `X-Real-IP`）——
  直接启动（无反向代理）时这是正确默认，但**反代后所有客户端会共用同一个限流桶**。
  要让登录/注册/改密/找回/上传按真实客户端分桶，代理必须正确设置 XFF，见下方示例。
- 默认只监听环回地址。需要对外提供服务时，显式设置 `PICBOX_ADDR` 与真实的
  `BASEURL`，并由已有反向代理终止 HTTPS；浏览器写请求只允许该源。

反向代理最小示例（Nginx，只列与本服务相关的部分）：

```nginx
location / {
    proxy_pass http://127.0.0.1:3001;
    proxy_set_header Host $host;                              # 链接与来源校验需要
    proxy_set_header X-Forwarded-Proto $scheme;               # 与 BASEURL 的 scheme 一致
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;  # 追加对端地址到最右端
}
```

`X-Forwarded-For` 必须**追加（append）或按真实对端覆盖**，把浏览器地址放在最右端；
不能把它设成常量或原样透传客户端提供的值。服务端只信任 `TRUSTED_PROXIES` 里的直连
对端，并从右往左剥离可信代理，所以左边的值是客户端自带、不会影响分桶。
新增代理层（例如又加了一层 CDN）时，把那一层的地址/网段显式加进 `TRUSTED_PROXIES`，
不要用 `0.0.0.0/0` 或整个内网段把可信范围放大；越界配置（含 `0.0.0.0/0`、非法
IP/CIDR）会让程序在打开数据库前启动失败。

`/etc/picbox.env` 示例（按实际域名与配额修改，不要提交到 Git）：

```sh
PICBOX_ADDR=127.0.0.1:3001
BASEURL=https://image.example.com
# 可选：可信反向代理的 IP/CIDR（逗号分隔，默认空 = 不信任任何转发头）。
# 本机反代保持 127.0.0.1/32,::1/128；不要写成 0.0.0.0/0 或整个内网段。
TRUSTED_PROXIES=127.0.0.1/32,::1/128
# JWT_SECRET 可选；不设置时首次启动随机生成到 /var/lib/picbox/auth.key（0600）

# 可选：账号默认配额（字节，0=不限；软删除仍计入；管理端可按账号覆盖）
USER_QUOTA_BYTES=0
# 可选：导出成功冷却小时数（0 关闭）与回收站保留天数（最小 1）
EXPORT_COOLDOWN_HOURS=12
TRASH_RETENTION_DAYS=30

# 完全可选：找回密码 / 修改邮箱。SMTP_HOST 与 SMTP_FROM 都设置（且上面的 BASEURL
# 非空）才启用；不配置时相关接口返回 503 MAIL_UNAVAILABLE，普通登录/上传/导出/管理端
# 不受影响，注册仍只依赖激活码（首个账号免激活码）。
SMTP_HOST=
SMTP_PORT=587
SMTP_SECURE=false
SMTP_REQUIRE_TLS=true
SMTP_USER=
SMTP_PASSWORD=
SMTP_FROM=
```

配置项以 `internal/server/config.go` 为准；`SMTP_*` 只配置一部分会启动失败，
避免「以为能发信、实际一直 503」的静默部署事故。

邮件默认就是关闭的：先把服务跑起来，直接注册首个账号（免激活码，自动成为管理员；
之后的账号才需要管理员生成的激活码），想启用找回密码/改邮箱时再补 `SMTP_*` 与
`BASEURL` 并 `sudo systemctl restart picbox`。服务**不会自动读取**
`/etc/picbox.env`——该文件由 unit 的 `EnvironmentFile=` 交给 systemd 注入；手工前台运行时
要自己 `export` 或按 `VAR=value ./picbox` 传入。

确认配置文件与目录就绪后，由管理员手动启用服务：

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now picbox
curl -fsS http://127.0.0.1:3001/api/health
```

`/api/health` 返回 `200` 且 `ok: true` 表示数据库可用；`503` 表示数据目录或数据库不可用，
先看 `journalctl -u picbox -n 100` 再决定是否继续。

## 4. 升级与回滚

升级同样全部手工执行。顺序固定为：**先做停服前的只读检查，再备份旧二进制与配置，
最后才停服备份数据**。任何一步检查失败都停在原地，不要继续往下做。

### 4.1 停服前的只读检查

```sh
# (a) 新包本体：校验与版本
sha256sum -c SHA256SUMS
./picbox_<version>_linux_amd64/picbox --version

# (b) 记录当前状态，回滚时要对照
#     注意：不要打印 /etc/picbox.env 的内容（可能包含 JWT_SECRET），只确认是否存在、可读
/usr/local/bin/picbox --version
systemctl cat picbox
if sudo test -r /etc/picbox.env; then
  echo "/etc/picbox.env: 存在且 root 可读（内容不打印）"
elif sudo test -e /etc/picbox.env; then
  echo "/etc/picbox.env: 存在但不可读，先修权限再继续，不要停服" >&2
  exit 1
else
  echo "/etc/picbox.env: 不存在（使用默认配置）"
fi

# (c) 备份目标可写，且剩余空间足够容纳一份数据目录与旧二进制
sudo test -d /var/backups && sudo test -w /var/backups
df -h /var/backups
sudo du -sh /var/lib/picbox
```

`/var/backups` 不可写、或可用空间不足以容纳“数据目录 + 旧二进制 + 配置”的至少 2 倍时，
停在原地处理，不要停服。

### 4.2 停服前备份旧二进制与配置

```sh
stamp=$(date -u +%Y%m%dT%H%M%SZ)
sudo install -d -m 0700 -o root -g root /var/backups/picbox

# 任何一份备份失败都必须停在原地：此时还没有停服，什么都没破坏
sudo cp -a /usr/local/bin/picbox /var/backups/picbox/picbox-$stamp \
  || { echo "备份旧二进制失败，停在原地，不要停服" >&2; exit 1; }
sudo cp -a /etc/systemd/system/picbox.service /var/backups/picbox/picbox.service-$stamp \
  || { echo "备份 unit 失败，停在原地，不要停服" >&2; exit 1; }

# /etc/picbox.env 可能存在也可能不存在（不存在=使用默认配置），
# 只有真正存在时才要求备份成功；不能用 2>/dev/null || true 吞掉读取失败
if sudo test -e /etc/picbox.env; then
  sudo cp -a /etc/picbox.env /var/backups/picbox/picbox.env-$stamp \
    || { echo "备份 /etc/picbox.env 失败，停在原地，不要停服" >&2; exit 1; }
fi

# 停服前逐个确认备份存在、非空且可读；有一份不满足就不要停服
backups=(/var/backups/picbox/picbox-$stamp /var/backups/picbox/picbox.service-$stamp)
if sudo test -e /etc/picbox.env; then
  backups+=(/var/backups/picbox/picbox.env-$stamp)
fi
for f in "${backups[@]}"; do
  # test -s/-r 只看元数据，再做一次真实读取，避免“看起来能读”实际读不出来
  if ! sudo test -s "$f" || ! sudo head -c 1 "$f" >/dev/null; then
    echo "备份缺失、为空或不可读: $f，停在原地，不要停服" >&2
    exit 1
  fi
done
sudo ls -l /var/backups/picbox
```

### 4.3 停服后备份数据目录

```sh
sudo systemctl stop picbox
sudo tar -C /var/lib -czf /var/backups/picbox/data-$stamp.tar.gz picbox
sudo tar -tzf /var/backups/picbox/data-$stamp.tar.gz | head   # 确认备份可读
```

数据备份没有完成或不可读时，不要替换二进制。

### 4.4 替换并启动

```sh
sudo install -m 0555 picbox_<version>_linux_amd64/picbox /usr/local/bin/.picbox.new
/usr/local/bin/.picbox.new --version
sudo mv /usr/local/bin/.picbox.new /usr/local/bin/picbox
sudo install -m 0644 picbox_<version>_linux_amd64/picbox.service /etc/systemd/system/picbox.service
sudo systemctl daemon-reload
sudo systemctl start picbox
curl -fsS http://127.0.0.1:3001/api/health
```

### 4.5 回滚

```sh
sudo systemctl stop picbox
sudo install -m 0555 /var/backups/picbox/picbox-$stamp /usr/local/bin/picbox
sudo install -m 0644 /var/backups/picbox/picbox.service-$stamp /etc/systemd/system/picbox.service
sudo systemctl daemon-reload
sudo systemctl start picbox
curl -fsS http://127.0.0.1:3001/api/health
```

只有当新版本已经改写过数据目录、且确认需要回到旧数据时，才用 4.3 的备份覆盖数据目录；
覆盖前先停服，并把当前数据另存一份，避免二次丢失。

不要把 PicBox 的数据目录与其他服务的数据库、`uploads/` 或旧版 schema 1 数据目录混用。
当前数据格式为 schema 2，**不支持就地升级**：服务会在任何改写之前拒绝外部数据库和
schema 1/未来版本的数据库。需要换目录或试用新数据时请指定一个全新的空目录
（例如 `PICBOX_DATA_DIR=/var/lib/picbox-new`），**不要删除或覆盖原有数据**；
要保留原有数据时先整体备份原目录，再决定是否另存迁移。

## 5. 关于 arm64

包内 `arm64` 二进制是在构建机（`amd64`）上交叉编译的静态 ELF，说明与构建脚本相同：
`CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath`。arm64 包只在
`arm64` Linux 运行机上执行，安装步骤与 `amd64` 完全一致（换成对应文件名）。
