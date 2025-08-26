# WebSSH Go

基于Go语言开发的WebSSH终端，支持Kubernetes Pod远程终端访问。

## 特性

- ✅ **Kubernetes集成**: 直接连接到Kubernetes Pod中的容器
- ✅ **多终端支持**: 支持bash和sh终端环境
- ✅ **实时通信**: 基于WebSocket的实时终端交互
- ✅ **身份验证**: 支持Token验证机制
- ✅ **健康检查**: 提供健康状态监控端点
- ✅ **监控指标**: 集成Prometheus监控指标
- ✅ **智能粘贴**: 支持长文本智能分块处理
- ✅ **进度反馈**: 长文本粘贴过程中提供实时进度反馈

## 🚀 长文本粘贴功能

### **问题解决**
解决了传统WebSSH在处理大量文本粘贴时出现的以下问题：
- 终端显示混乱
- 内容被截断或覆盖
- 缓冲区溢出导致的连接断开
- 粘贴过程中无法感知进度

### **智能处理机制**

1. **自动检测**: 自动识别长文本和多行文本
2. **分块传输**: 将大文本分成小块逐步传输
3. **速率控制**: 控制传输速度避免缓冲区溢出
4. **用户确认**: 大文本粘贴前可选择确认机制
5. **进度反馈**: 实时显示粘贴进度
6. **错误恢复**: 粘贴失败时提供错误信息

### **配置参数**

```yaml
# 长文本处理配置
paste:
  max_chunk_size: 1024        # 每次写入的最大字节数
  write_delay_ms: 10          # 写入间隔（毫秒）
  confirm_threshold: 1024     # 超过此大小的文本需要用户确认
  max_paste_size: 1048576     # 最大允许粘贴大小（1MB）
  enable_confirmation: true   # 是否启用粘贴确认
```

### **前端集成指南**

#### **WebSocket消息类型**

```javascript
// 长文本粘贴相关消息类型
const messageTypes = {
    // 客户端发送
    input: "input",              // 普通输入
    paste_confirm: "paste_confirm", // 确认粘贴
    
    // 服务端发送
    paste_confirm: "paste_confirm",  // 粘贴确认请求
    paste_start: "paste_start",      // 粘贴开始
    paste_progress: "paste_progress", // 粘贴进度
    paste_complete: "paste_complete", // 粘贴完成
    paste_error: "paste_error"       // 粘贴错误
};
```

#### **前端处理示例**

```javascript
// 1. 监听粘贴确认
ws.onmessage = function(event) {
    const msg = JSON.parse(event.data);
    
    switch(msg.type) {
        case 'paste_confirm':
            // 显示确认对话框
            if (confirm(msg.message)) {
                // 用户确认，发送粘贴指令
                ws.send(JSON.stringify({
                    type: 'paste_confirm',
                    data: msg.data
                }));
            }
            break;
            
        case 'paste_start':
            console.log('开始粘贴:', msg.message);
            showProgressBar();
            break;
            
        case 'paste_progress':
            console.log('粘贴进度:', msg.message);
            updateProgressBar(msg.message);
            break;
            
        case 'paste_complete':
            console.log('粘贴完成:', msg.message);
            hideProgressBar();
            break;
            
        case 'paste_error':
            console.error('粘贴错误:', msg.message);
            hideProgressBar();
            showError(msg.message);
            break;
    }
};

// 2. 智能粘贴检测
function handlePaste(pastedText) {
    // 发送到WebSSH，自动检测是否需要分块处理
    ws.send(JSON.stringify({
        type: 'input',
        data: pastedText
    }));
}

// 3. 进度条UI示例
function showProgressBar() {
    const progressDiv = document.createElement('div');
    progressDiv.id = 'paste-progress';
    progressDiv.innerHTML = `
        <div style="background: rgba(0,0,0,0.8); color: white; padding: 10px; 
                    position: fixed; top: 50%; left: 50%; transform: translate(-50%, -50%);
                    border-radius: 5px; z-index: 1000;">
            <div>正在粘贴...</div>
            <div id="progress-text">准备中...</div>
            <div style="width: 300px; height: 20px; background: #333; margin-top: 10px;">
                <div id="progress-bar" style="height: 100%; background: #4CAF50; width: 0%; transition: width 0.3s;"></div>
            </div>
        </div>
    `;
    document.body.appendChild(progressDiv);
}

function updateProgressBar(message) {
    const progressText = document.getElementById('progress-text');
    const progressBar = document.getElementById('progress-bar');
    
    if (progressText) progressText.textContent = message;
    
    // 提取百分比
    const match = message.match(/(\d+)%/);
    if (match && progressBar) {
        progressBar.style.width = match[1] + '%';
    }
}

function hideProgressBar() {
    const progressDiv = document.getElementById('paste-progress');
    if (progressDiv) {
        progressDiv.remove();
    }
}
```

#### **最佳实践**

1. **粘贴检测**: 监听`paste`事件自动处理
2. **用户体验**: 提供清晰的进度反馈和确认机制
3. **错误处理**: 优雅处理粘贴失败情况
4. **性能优化**: 根据网络情况调整`write_delay_ms`参数

## 快速开始

### 1. 配置文件

创建 `config.yaml`:

```yaml
server:
  address: ":8080"

k8s:
  config_path: ""  # 留空使用集群内配置

auth:
  verify_token_url: "http://your-api/verify"
  timeout: 5

paste:
  max_chunk_size: 1024
  write_delay_ms: 10
  confirm_threshold: 1024
  max_paste_size: 1048576
  enable_confirmation: true
```

### 2. 启动服务

```bash
./webssh-go
```

### 3. WebSocket连接

```javascript
const ws = new WebSocket('ws://localhost:8080/ws?namespace=default&podName=my-pod&containerName=my-container&x-token=your-token');
```

## 环境变量

所有配置都可以通过环境变量覆盖：

```bash
export WEBSSH_SERVER_ADDRESS=":8080"
export WEBSSH_AUTH_VERIFY_TOKEN_URL="http://your-api/verify"
export WEBSSH_PASTE_MAX_CHUNK_SIZE="2048"
export WEBSSH_PASTE_ENABLE_CONFIRMATION="false"
```

## 监控端点

- **健康检查**: `GET /healthz`
- **监控指标**: `GET /metrics`

## 许可证

MIT License
