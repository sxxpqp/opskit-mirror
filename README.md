# opskit-mirror

Go 纯镜像转发服务，无磁盘镜像缓存。公网域名仅在 Nginx 配置，Go 从 Host 和可信 X-Forwarded-Proto 生成鉴权入口。只支持 GET/HEAD，不支持 push。后端仅允许可信 Nginx 访问。

## 运行

Go 1.26.2：

```sh
cp config.example.yaml config.yaml
go run . --config config.yaml --check
go run . --config config.yaml
```

默认监听 :8080（所有网络接口）。/healthz 仅表示进程存活。

Docker Compose 直接使用默认配置；宿主机端口仍仅发布到本机，供 Nginx 转发。

```sh
# 默认 latest；可选固定版本：export MIRROR_VERSION=<commit-SHA或版本标签>
docker compose config --images
docker compose pull
docker compose up -d
```

Compose 默认使用 latest，pull_policy: always 在每次 docker compose up 时检查拉取镜像。可用 MIRROR_VERSION 覆盖为 commit SHA 或版本标签。配置文件可选挂载，修改后重建容器。

## Nginx 与地址

deploy/nginx/mirror.conf 仅使用 hub.sxxpqp.top。配置该域名的 DNS 和 HTTPS 证书，已有网站请合并 location。nginx -t 通过后 reload。必须转发 /auth/ 和 /v2/。

- /v2/ 默认 Docker Hub 标准入口。
- /v2/dockerhub/、/v2/ghcr/、/v2/quay/、/v2/k8s/ 为路径入口。
- 认证 realm 改写到同入口 /auth/；service/scope 保留。
- manifest 和镜像层在服务端跟随任意公网 HTTPS CDN 重定向，最多 5 次；不携带 Authorization 或 Cookie，保留 Range、Accept、If-Range。
- auth_hosts 继续严格匹配认证域名；CDN 重定向不再使用域名白名单。拒绝 HTTP、带用户名/密码的 URL、内网、回环、链路本地、共享地址和云元数据地址。所有出站连接先校验全部 DNS 地址，再连接校验过的 IP，防止 DNS 重绑定。
- 为保证 IP 校验作用于实际目标，出站连接不使用 HTTP_PROXY、HTTPS_PROXY 或 ALL_PROXY；上游及认证服务也必须解析到公网地址。旧 redirect_hosts / MIRROR_<仓库名大写>_REDIRECT_HOSTS 兼容读取但不再影响放行规则，无需维护 CDN 域名。

## Docker 客户端配置

以下操作在使用镜像的客户端执行，代理服务器需先配置好 Nginx、DNS 和可信 HTTPS 证书。

### Linux Docker Engine

将下面字段合并进 `/etc/docker/daemon.json`，保留已有配置；不存在时创建文件：

```json
{
  "registry-mirrors": ["https://hub.sxxpqp.top"]
}
```

使用域名根地址，不填写 `/v2/dockerhub`。只加速 Docker Hub，不能靠此字段映射 GHCR、Quay 等其他仓库。验证配置后，在维护窗口重启 Docker（可能影响运行中的容器）：

```sh
sudo dockerd --validate --config-file=/etc/docker/daemon.json
sudo systemctl restart docker
docker info --format '{{json .RegistryConfig.Mirrors}}'
docker pull docker.io/library/busybox:1.36.1
```

Docker Desktop：Settings → Docker Engine，将上述字段合并到 JSON，点击 Apply & restart，再执行拉取命令。不要修改 Desktop 虚拟机内部配置文件。

仍使用原始镜像名，不需要改业务 YAML。也可显式测试标准入口：

```sh
docker pull hub.sxxpqp.top/library/busybox:1.36.1
```

固定上游账号在代理服务端配置，客户端不需要为此执行 docker login。私有镜像及客户端凭据透传另行验证。

## containerd 客户端配置

以下为独立 Linux containerd 配置。K3s、RKE2 等发行版可能自动生成配置，需使用发行版自己的配置入口。先执行 `containerd --version` 确认主版本。

### 1. 启用 hosts 配置目录

编辑 `/etc/containerd/config.toml`，合并对应段落，避免重复 TOML 表，不要覆盖现有运行时、CNI、snapshotter 等配置。

containerd 1.x（1.5+，version=2 格式）：

```toml
version = 2
[plugins."io.containerd.grpc.v1.cri".registry]
  config_path = "/etc/containerd/certs.d"
```

containerd 2.x（version=3 格式）：

```toml
version = 3
[plugins."io.containerd.cri.v1.images".registry]
  config_path = "/etc/containerd/certs.d"
```

不要只把旧配置的 version 数字改成 3；旧格式需先按 containerd 官方方式迁移。首次修改 config.toml 后在维护窗口重启并检查：

```sh
sudo systemctl restart containerd
sudo systemctl status containerd --no-pager
```

### 2. Docker Hub 域名入口（推荐）

```sh
sudo mkdir -p /etc/containerd/certs.d/docker.io
sudo tee /etc/containerd/certs.d/docker.io/hosts.toml >/dev/null <<'TOML'
server = "https://registry-1.docker.io"
[host."https://hub.sxxpqp.top"]
  capabilities = ["pull", "resolve"]
TOML
```

### 3. 路径入口（与域名入口二选一）

同一个 docker.io/hosts.toml 也可以改为：

```toml
server = "https://registry-1.docker.io"
[host."https://hub.sxxpqp.top/v2/dockerhub"]
  capabilities = ["pull", "resolve"]
  override_path = true
```

只有包含自定义 API 路径的入口才设置 override_path=true。更新 hosts.toml 无需重启 containerd。保留官方 server 时，代理失败可能回退直连，所以仅看到拉取成功不能证明用了代理。

其他源在相应目录创建独立 hosts.toml，格式同上并设置 override_path=true：

| 配置目录（/etc/containerd/certs.d 下） | server | host 路径入口 |
| --- | --- | --- |
| ghcr.io | https://ghcr.io | https://hub.sxxpqp.top/v2/ghcr |
| quay.io | https://quay.io | https://hub.sxxpqp.top/v2/quay |
| registry.k8s.io | https://registry.k8s.io | https://hub.sxxpqp.top/v2/k8s |

下面三组命令可直接执行，与 Docker Hub 配置并存；镜像名保持原始仓库地址。

```sh
sudo mkdir -p /etc/containerd/certs.d/ghcr.io
sudo tee /etc/containerd/certs.d/ghcr.io/hosts.toml >/dev/null <<'TOML'
server = "https://ghcr.io"
[host."https://hub.sxxpqp.top/v2/ghcr"]
  capabilities = ["pull", "resolve"]
  override_path = true
TOML

sudo mkdir -p /etc/containerd/certs.d/quay.io
sudo tee /etc/containerd/certs.d/quay.io/hosts.toml >/dev/null <<'TOML'
server = "https://quay.io"
[host."https://hub.sxxpqp.top/v2/quay"]
  capabilities = ["pull", "resolve"]
  override_path = true
TOML

sudo mkdir -p /etc/containerd/certs.d/registry.k8s.io
sudo tee /etc/containerd/certs.d/registry.k8s.io/hosts.toml >/dev/null <<'TOML'
server = "https://registry.k8s.io"
[host."https://hub.sxxpqp.top/v2/k8s"]
  capabilities = ["pull", "resolve"]
  override_path = true
TOML
```

这些命令会替换对应 hosts.toml；已有自定义配置时先备份或合并。配置目录必须与镜像原始仓库名一致：Kubernetes 官方源是 registry.k8s.io，不是 k8s。旧 k8s.gcr.io 不会自动匹配此配置。

上述源接入方式已提供，真实连通、鉴权和 CDN 下载仍需逐个验证。

### 4. 验证 containerd / Kubernetes

ctr 必须显式指定 --hosts-dir；普通 ctr 不会自动继承 CRI 的配置：

```sh
sudo ctr -n k8s.io images pull --hosts-dir /etc/containerd/certs.d docker.io/library/busybox:1.36.1
```

已安装 crictl 的节点可验证 Kubernetes 实际使用的 CRI 路径：

```sh
sudo crictl --runtime-endpoint unix:///run/containerd/containerd.sock pull docker.io/library/busybox:1.36.1
```

socket 路径按节点实际配置调整。每台 Kubernetes 工作节点都需要配置；imagePullPolicy: Always 不会强制重新下载已存在的镜像层。

## opskit 配置

合并至 opskit 的配置文件：

```yaml
mirrors:
  docker:
    - https://hub.sxxpqp.top
  containerd:
    docker.io:
      server: https://registry-1.docker.io
      endpoint: https://hub.sxxpqp.top/v2/dockerhub
      override_path: true
      skip_verify: false
    ghcr.io:
      server: https://ghcr.io
      endpoint: https://hub.sxxpqp.top/v2/ghcr
      override_path: true
      skip_verify: false
```

## 确认请求经过代理

在代理项目目录查看：

```sh
docker compose logs -f mirror
```

再从客户端拉取镜像。token 200、manifest 200、blob 200/206 表示对应请求成功；首次 401 通常是正常的 Bearer 认证挑战。已缓存镜像层可能不再请求 blob，可选用客户端尚未拉取过的镜像验证，不必删除业务镜像。最终结合客户端完成提示确认拉取结果。

用户已验证 Docker Hub 的 Docker 加速源拉取；containerd 和其他源尚未实测。排查：无日志先查 DNS/Nginx；502 查上游网络、auth_hosts 及公网地址校验；证书错误应修复证书链或安装 CA，不默认关闭证书验证。

参考：[Docker 官方 mirror 配置](https://docs.docker.com/docker-hub/image-library/mirror/)、[containerd 官方 hosts 配置](https://github.com/containerd/containerd/blob/main/docs/hosts.md)。

## 登录边界

提供 Bearer 鉴权转发，默认不配置共享上游账号。镜像 API 不透传 Basic 凭据，认证端点只允许配置的 auth_hosts。docker login docker.io 与 docker login hub.sxxpqp.top 是不同身份，不能承诺 mirror 自动继承私有凭据；Docker 登录、私有镜像、Kubernetes imagePullSecrets 需分别上线实测。

Nginx/防火墙限制访问；Go 没有内置用户管理。无整层内存缓冲和磁盘缓存，正常网络缓冲仍存在。上游响应头超时 900 秒，下载无固定总时限，客户端断开取消上游。日志不输出凭据及签名 URL。

## CI

沿用 mysql-s3-backup：PR 测试；main、v* tag、手动触发发布 linux/amd64 阿里云镜像。main 发布 latest、完整 SHA，tag 发布版本标签。Compose 默认 latest，可指定版本。

Secrets：ALIYUN_REGISTRY、ALIYUN_NAME_SPACE、ALIYUN_REGISTRY_USER、ALIYUN_REGISTRY_PASSWORD，不会跨仓库继承。镜像名 opskit-mirror。

```sh
go test -race ./...
go vet ./...
```

单元测试使用模拟上游，不等同真实上游、Nginx、Docker/containerd 验证。

### 可选 Docker Hub 固定账号

```yaml
registries:
  dockerhub:
    upstream: https://registry-1.docker.io
    username: your-dockerhub-user
    password_env: DOCKERHUB_TOKEN
    auth_hosts: [auth.docker.io]
```

DOCKERHUB_TOKEN 环境变量填写 Docker Hub 只读访问令牌，不把真实凭据提交到仓库。Compose 从宿主环境或 .env 读取该变量；本机运行先 export。用户名和 password_env 必须成对配置，令牌为空启动失败。固定账号仅用于白名单 token 端点并覆盖客户端凭据，不发送给镜像层 CDN。该账号可访问的私有镜像可能被代理使用者获取，启用前必须限制 Nginx/网络访问。否则省略这两个字段，由客户端提供认证；私有镜像仍需实测。

## 无挂载与环境变量配置

config.example.yaml 编译内置，直接运行无需挂载；显式 --config 指定文件不存在会报错。优先级：内置默认 < 显式 YAML < 环境变量；YAML registries 若提供，则替换默认仓库集合。不会自动读取工作目录 config.yaml。

```sh
MIRROR_LISTEN=:8080 MIRROR_DOCKERHUB_USERNAME=your-user MIRROR_DOCKERHUB_PASSWORD=your-token ./opskit-mirror
```

Compose 默认无需配置文件。可在 .env 设置 MIRROR_DOCKERHUB_USERNAME、MIRROR_DOCKERHUB_PASSWORD；匿名访问两者留空。MIRROR_VERSION 可选，默认 latest。要挂载 YAML，取消 compose.yaml 的 command 和 volumes 注释。

所有仓库支持 MIRROR_<仓库名大写>_UPSTREAM、USERNAME、PASSWORD、PASSWORD_ENV、AUTH_HOSTS；列表用逗号分隔。PASSWORD 为直接凭据环境变量；PASSWORD_ENV 指向另一个环境变量，二者同时存在时 PASSWORD 优先。环境变量只覆盖已配置仓库，不隐式创建新仓库。容器使用额外变量时需在 Compose environment 显式传入，或 docker run -e。

固定账号权限对代理使用者共享，公网必须限制入口。

## 日志

访问日志为 JSON：request_id、registry、operation（token/manifest/blob）、method、status、bytes、duration_ms、result。上游失败日志关联相同 request_id；响应头返回 X-Request-ID。客户端取消单独标记，不输出 Authorization、token、密码、完整查询参数或签名下载地址。健康检查不记录访问日志。启动/配置错误另有进程日志。日志记录本次传输字节，不代表镜像总大小或固定百分比；Docker/containerd 实际拉取仍需联调。

重定向校验失败会额外记录 redirect_denied，包含 request_id、registry、redirect_host、redirect_scheme 和固定原因；不记录完整重定向 URL、路径或查询参数。结合相同 request_id 的 upstream_failed 定位被拒绝的重定向；连接阶段因非公网地址失败时 reason 为 destination address is not public。
