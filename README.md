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
export MIRROR_VERSION=<已发布的完整commit-SHA或版本标签>
docker compose config --images
docker compose pull
docker compose up -d
```

必须显式指定版本，不默认 latest。配置持久保存在宿主机，修改后重建容器。

## Nginx 与地址

deploy/nginx/mirror.conf 包含 sxxpqp.top、hub.sxxpqp.top。配置 DNS 和覆盖两个域名的证书，已有网站请合并 location。nginx -t 通过后 reload。必须转发 /auth/ 和 /v2/。

- /v2/ 默认 Docker Hub 标准入口。
- /v2/dockerhub/、/v2/ghcr/、/v2/quay/、/v2/k8s/ 为路径入口。
- 认证 realm 改写到同入口 /auth/；service/scope 保留。
- 镜像层服务端跟随允许的 CDN 重定向，不携带 Authorization，保留 Range。
- auth_hosts 和 redirect_hosts 严格匹配域名，未知目标返回 502。示例清单可能不覆盖所有地区，需按实际官方重定向补充，不使用通配开放代理。

## containerd / opskit

/etc/containerd/certs.d/docker.io/hosts.toml：

```toml
server = "https://registry-1.docker.io"
[host."https://sxxpqp.top/v2/dockerhub"]
  capabilities = ["pull", "resolve"]
  override_path = true
```

CRI registry.config_path 需指向 /etc/containerd/certs.d，1.x/2.x 插件配置位置不同。ctr 使用 --hosts-dir /etc/containerd/certs.d。也可用 https://hub.sxxpqp.top 并去掉 override_path。

opskit：endpoint=https://sxxpqp.top/v2/dockerhub、override_path=true、skip_verify=false。保留官方 server 时，失败可能直连回退。

Docker daemon.json 合并以下内容，再按维护窗口重启 Docker：

```json
{"registry-mirrors":["https://hub.sxxpqp.top"]}
```

Docker 使用域名根地址，registry-mirrors 主要用于 Docker Hub。

## 登录边界

提供 Bearer 鉴权转发，默认不配置共享上游账号。镜像 API 不透传 Basic 凭据，认证端点只允许配置的 auth_hosts。docker login docker.io 与 docker login hub.sxxpqp.top 是不同身份，不能承诺 mirror 自动继承私有凭据；Docker 登录、私有镜像、Kubernetes imagePullSecrets 需分别上线实测。

Nginx/防火墙限制访问；Go 没有内置用户管理。无整层内存缓冲和磁盘缓存，正常网络缓冲仍存在。上游响应头超时 900 秒，下载无固定总时限，客户端断开取消上游。日志不输出凭据及签名 URL。

## CI

沿用 mysql-s3-backup：PR 测试；main、v* tag、手动触发发布 linux/amd64 阿里云镜像。main 发布 latest、完整 SHA，tag 发布版本标签。部署要求显式版本。

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
    redirect_hosts: [production.cloudflare.docker.com, production.cloudfront.docker.com]
```

DOCKERHUB_TOKEN 环境变量填写 Docker Hub 只读访问令牌，不把真实凭据提交到仓库。Compose 从宿主环境或 .env 读取该变量；本机运行先 export。用户名和 password_env 必须成对配置，令牌为空启动失败。固定账号仅用于白名单 token 端点并覆盖客户端凭据，不发送给镜像层 CDN。该账号可访问的私有镜像可能被代理使用者获取，启用前必须限制 Nginx/网络访问。否则省略这两个字段，由客户端提供认证；私有镜像仍需实测。
