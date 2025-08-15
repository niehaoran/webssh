# WebSSH-Go

WebSSH-Go 是一个功能强大的 Web 终端服务，允许通过浏览器连接到 Kubernetes 集群中的 Pod 容器，以及提供集群级别的管理能力。

## 功能特点

- 通过浏览器访问 Kubernetes Pod 容器的终端
- 支持用户认证和授权
- 支持终端调整大小、命令历史记录和自动补全
- 支持 Tab 补全和控制键(Ctrl+C, Ctrl+D 等)
- 支持连接到 KubeSphere 的 kubectl 管理 Pod

## 快速开始

### 构建

```bash
go build -o webssh-go .
```

### 运行

```bash
./webssh-go
```

### Docker 部署

```bash
# 使用默认配置运行
docker run -p 8080:8080 your-registry/webssh-go:latest

# 使用环境变量设置验证URL（推荐方式）
docker run -p 8080:8080 \
  -e WEBSSH_AUTH_VERIFY_TOKEN_URL="https://your-auth-service.com/verify" \
  your-registry/webssh-go:latest

# 使用多个环境变量进行完整配置
docker run -p 8080:8080 \
  -e WEBSSH_AUTH_VERIFY_TOKEN_URL="https://your-auth-service.com/verify" \
  -e WEBSSH_AUTH_TIMEOUT="10" \
  -e WEBSSH_SERVER_ADDRESS=":8080" \
  -e WEBSSH_KUBERNETES_CONFIG="/path/to/kubeconfig" \
  your-registry/webssh-go:latest

# 在Kubernetes中部署时，可以在deployment.yaml中设置环境变量：
# env:
# - name: WEBSSH_AUTH_VERIFY_TOKEN_URL
#   value: "https://your-auth-service.com/verify"
```

## 配置方法

WebSSH-Go 支持两种配置方式：

1. **配置文件** - 使用`config.yaml`
2. **环境变量** - 以`WEBSSH_`为前缀（优先级高于配置文件）

### 配置文件

`config.yaml` 文件用于配置 WebSSH-Go 服务：

```yaml
server:
  address: ":8080" # 服务监听地址

kubernetes:
  config: "/etc/kubernetes/kubeconfig" # Kubernetes配置路径
  kubectl:
    path: "/usr/local/bin/kubectl" # kubectl命令路径
    default_namespace: "default" # 默认命名空间

terminal:
  shell:
    history_size: 5000 # 历史命令数量
    completion: true # 命令自动补全
    vi_mode: true # vi编辑模式
    default_shell: "bash" # 默认shell

auth:
  verify_token_url: "http://api.janzhi.cn/applicationCloud/verifyWebsshToken" # 默认token验证地址
  timeout: 5 # 验证超时时间(秒)
```

### 环境变量

所有配置都可以通过环境变量覆盖，环境变量名称规则为：

- 前缀: `WEBSSH_`
- 配置路径: 将`.`替换为`_`

主要环境变量:

| 环境变量                       | 对应配置                | 说明             |
| ------------------------------ | ----------------------- | ---------------- |
| `WEBSSH_AUTH_VERIFY_TOKEN_URL` | `auth.verify_token_url` | 验证服务 URL     |
| `WEBSSH_AUTH_TIMEOUT`          | `auth.timeout`          | 验证超时时间     |
| `WEBSSH_SERVER_ADDRESS`        | `server.address`        | 服务监听地址     |
| `WEBSSH_KUBERNETES_CONFIG`     | `kubernetes.config`     | K8s 配置文件路径 |

## 连接参数

### 连接 Pod 终端

```
ws://webssh服务地址:8080/ws?x-token=用户令牌&x-user-id=用户ID&clusterId=集群ID&namespace=命名空间&podName=Pod名称&containerName=容器名称
```

### 连接 kubectl 管理 Pod

```
ws://webssh服务地址:8080/ws?x-token=用户令牌&x-user-id=用户ID&clusterId=集群ID&namespace=kube-system&podName=ks-managed-kubectl-admin&containerName=kubectl
```

## WebSocket 消息格式

### 初始化

```json
{
  "type": "init",
  "namespace": "default",
  "podName": "my-pod",
  "containerName": "my-container"
}
```

### 输入命令

```json
{
  "type": "input",
  "data": "ls -la\n"
}
```

### 调整终端大小

```json
{
  "type": "resize",
  "cols": 80,
  "rows": 24
}
```

### 发送控制键

```json
{
  "type": "ctrl",
  "ctrlKey": "c"
}
```

## 安全建议

- 仅允许授权用户访问 WebSSH 服务
- 为 kubectl 管理 Pod 提供严格的访问控制
- 启用审计日志记录命令执行情况
- 定期更新 kubeconfig 和权限设置

## 权限控制

确保 WebSSH-Go 服务器上的 kubeconfig 具有适当的权限，并且验证服务正确验证用户身份和资源访问权限。

## 监控

WebSSH-Go 提供了 Prometheus 指标，可通过`/metrics`端点访问：

- `active_websocket_connections` - 当前活跃的 WebSocket 连接数
- `commands_executed_total` - 执行的命令总数

## 贡献

欢迎提交问题和贡献代码！
