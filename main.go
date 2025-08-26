package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/viper"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
)

var (
	logger    *zap.Logger
	config    *rest.Config
	clientset *kubernetes.Clientset

	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}

	activeConnections = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "active_websocket_connections",
		Help: "Number of active WebSocket connections",
	})

	commandsExecuted = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "commands_executed_total",
		Help: "Total number of commands executed",
	})

	messagePool = sync.Pool{
		New: func() interface{} {
			return new(Message)
		},
	}
)

type Message struct {
	Type          string `json:"type"`
	Token         string `json:"token,omitempty"`
	UserId        string `json:"userId,omitempty"`
	Namespace     string `json:"namespace,omitempty"`
	PodName       string `json:"podName,omitempty"`
	ContainerName string `json:"containerName,omitempty"`
	Command       string `json:"command,omitempty"`
	Data          string `json:"data,omitempty"`
	Message       string `json:"message,omitempty"`
	Cols          uint16 `json:"cols,omitempty"`
	Rows          uint16 `json:"rows,omitempty"`
	CtrlKey       string `json:"ctrlKey,omitempty"`
	// 添加粘贴相关字段
	PasteId      string `json:"pasteId,omitempty"`
	ChunkIndex   int    `json:"chunkIndex,omitempty"`
	TotalChunks  int    `json:"totalChunks,omitempty"`
	IsLargePaste bool   `json:"isLargePaste,omitempty"`
}

type PodDetails struct {
	Namespace     string
	PodName       string
	ContainerName string
}

type TerminalSession struct {
	conn        *websocket.Conn
	stdinReader io.Reader
	sizeChan    chan remotecommand.TerminalSize
	lastSize    remotecommand.TerminalSize
	sizeMutex   sync.Mutex
	tty         bool
	done        chan struct{}
	env         map[string]string
	// 添加长文本处理相关字段
	inputBuffer  chan []byte
	isProcessing bool
	processMutex sync.Mutex
}

func (t *TerminalSession) Next() *remotecommand.TerminalSize {
	t.sizeMutex.Lock()
	defer t.sizeMutex.Unlock()

	select {
	case size := <-t.sizeChan:
		t.lastSize = size
		return &size
	case <-t.done:
		return nil
	}
}

func (t *TerminalSession) Read(p []byte) (int, error) {
	return t.stdinReader.Read(p)
}

func (t *TerminalSession) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	msg := &Message{
		Type: "output",
		Data: string(p),
	}
	err := t.conn.WriteJSON(msg)
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

func (t *TerminalSession) initializeEnv() {
	t.env = map[string]string{
		"TERM":         "xterm-256color",
		"COLORTERM":    "truecolor",
		"LANG":         "en_US.UTF-8",
		"LC_ALL":       "en_US.UTF-8",
		"HISTCONTROL":  "ignoreboth",
		"HISTSIZE":     "1000",
		"HISTFILESIZE": "2000",
	}
}

func (t *TerminalSession) Close() {
	close(t.done)
	if t.inputBuffer != nil {
		close(t.inputBuffer)
	}
	t.conn.Close()
}

// 添加长文本分块处理方法
var (
	maxChunkSize       int           // 从配置读取
	writeDelay         time.Duration // 从配置读取
	confirmThreshold   int           // 从配置读取
	maxPasteSize       int           // 从配置读取
	enableConfirm      bool          // 从配置读取
	useBracketPaste    bool          // 从配置读取
	cleanControlChars  bool          // 从配置读取
	multilineThreshold int           // 从配置读取
)

// 初始化粘贴配置
func initPasteConfig() {
	maxChunkSize = viper.GetInt("paste.max_chunk_size")
	if maxChunkSize <= 0 {
		maxChunkSize = 512 // 默认512字节
	}

	writeDelayMs := viper.GetInt("paste.write_delay_ms")
	if writeDelayMs <= 0 {
		writeDelayMs = 20 // 默认20ms
	}
	writeDelay = time.Duration(writeDelayMs) * time.Millisecond

	confirmThreshold = viper.GetInt("paste.confirm_threshold")
	if confirmThreshold <= 0 {
		confirmThreshold = 512 // 默认512字符
	}

	maxPasteSize = viper.GetInt("paste.max_paste_size")
	if maxPasteSize <= 0 {
		maxPasteSize = 1048576 // 1MB
	}

	enableConfirm = viper.GetBool("paste.enable_confirmation")
	useBracketPaste = viper.GetBool("paste.use_bracket_paste")
	cleanControlChars = viper.GetBool("paste.clean_control_chars")

	multilineThreshold = viper.GetInt("paste.multiline_threshold")
	if multilineThreshold <= 0 {
		multilineThreshold = 2
	}

	logger.Info("Paste configuration loaded",
		zap.Int("max_chunk_size", maxChunkSize),
		zap.Duration("write_delay", writeDelay),
		zap.Int("confirm_threshold", confirmThreshold),
		zap.Bool("enable_confirmation", enableConfirm),
		zap.Bool("use_bracket_paste", useBracketPaste))
}

// processLongText 处理长文本，分块写入stdin
func (t *TerminalSession) processLongText(data string, stdinWriter *io.PipeWriter) {
	t.processMutex.Lock()
	if t.isProcessing {
		t.processMutex.Unlock()
		return
	}
	t.isProcessing = true
	t.processMutex.Unlock()

	defer func() {
		t.processMutex.Lock()
		t.isProcessing = false
		t.processMutex.Unlock()
	}()

	// 清理和预处理数据
	cleanData := t.cleanPasteData(data)
	dataBytes := []byte(cleanData)
	dataLen := len(dataBytes)

	// 如果数据较小，直接写入
	if dataLen <= maxChunkSize {
		t.writePasteData(stdinWriter, dataBytes)
		return
	}

	// 发送开始信息
	startMsg := &Message{
		Type:    "paste_start",
		Message: fmt.Sprintf("开始粘贴 %d 字符...", dataLen),
	}
	t.conn.WriteJSON(startMsg)

	// 进入粘贴模式：发送^[[200~ (bracket paste mode start)
	if useBracketPaste {
		stdinWriter.Write([]byte("\x1b[200~"))
	}

	// 分块处理大文本
	totalChunks := (dataLen + maxChunkSize - 1) / maxChunkSize
	for i := 0; i < dataLen; i += maxChunkSize {
		select {
		case <-t.done:
			if useBracketPaste {
				stdinWriter.Write([]byte("\x1b[201~"))
			}
			return
		default:
			end := i + maxChunkSize
			if end > dataLen {
				end = dataLen
			}

			chunk := dataBytes[i:end]
			_, err := stdinWriter.Write(chunk)
			if err != nil {
				logger.Error("Failed to write chunk to stdin", zap.Error(err))
				errMsg := &Message{
					Type:    "paste_error",
					Message: "粘贴失败: " + err.Error(),
				}
				t.conn.WriteJSON(errMsg)
				// 退出粘贴模式
				if useBracketPaste {
					stdinWriter.Write([]byte("\x1b[201~"))
				}
				return
			}

			// 发送进度信息
			currentChunk := (i / maxChunkSize) + 1
			if currentChunk%10 == 0 || currentChunk == totalChunks {
				progressMsg := &Message{
					Type:    "paste_progress",
					Message: fmt.Sprintf("粘贴进度: %d/%d (%d%%)", currentChunk, totalChunks, (currentChunk*100)/totalChunks),
				}
				t.conn.WriteJSON(progressMsg)
			}

			// 添加延迟，避免终端缓冲区溢出
			if i+maxChunkSize < dataLen {
				time.Sleep(writeDelay)
			}
		}
	}

	// 退出粘贴模式：发送^[[201~ (bracket paste mode end)
	if useBracketPaste {
		stdinWriter.Write([]byte("\x1b[201~"))
	}

	// 发送完成信息
	completeMsg := &Message{
		Type:    "paste_complete",
		Message: fmt.Sprintf("粘贴完成! 共处理 %d 字符", dataLen),
	}
	t.conn.WriteJSON(completeMsg)
}

// 安全地写入粘贴数据
func (t *TerminalSession) writePasteData(stdinWriter *io.PipeWriter, data []byte) error {
	if useBracketPaste {
		// 进入粘贴模式
		stdinWriter.Write([]byte("\x1b[200~"))
	}

	// 写入数据
	_, err := stdinWriter.Write(data)

	if useBracketPaste {
		// 退出粘贴模式
		stdinWriter.Write([]byte("\x1b[201~"))
	}

	return err
}

// 清理粘贴数据，处理特殊字符
func (t *TerminalSession) cleanPasteData(data string) string {
	if !cleanControlChars {
		return data
	}

	// 移除或转义可能导致问题的控制字符
	cleaned := strings.ReplaceAll(data, "\r\n", "\n") // 统一换行符
	cleaned = strings.ReplaceAll(cleaned, "\r", "\n") // 处理Mac格式换行

	// 移除可能导致终端混乱的控制序列
	re := regexp.MustCompile(`\x1b\[[0-9;]*[mK]`) // 移除ANSI颜色码
	cleaned = re.ReplaceAllString(cleaned, "")

	// 移除其他危险的控制字符，但保留换行符和制表符
	var result strings.Builder
	for _, r := range cleaned {
		if r == '\n' || r == '\t' || (r >= 32 && r <= 126) || r > 126 {
			result.WriteRune(r)
		}
	}

	return result.String()
}

// 检查数据是否需要清理（包含特殊字符或控制序列）
func (t *TerminalSession) needsCleanup(data string) bool {
	if !cleanControlChars {
		return false
	}

	// 检查是否包含控制字符或ANSI序列
	for _, r := range data {
		if r < 32 && r != '\t' && r != '\n' {
			return true
		}
	}

	// 检查是否包含ANSI转义序列
	return strings.Contains(data, "\x1b[")
}

// 检测是否为长文本粘贴
func (t *TerminalSession) isLongText(data string) bool {
	lineCount := strings.Count(data, "\n") + 1
	return len(data) > confirmThreshold || lineCount >= multilineThreshold
}

func init() {
	var err error
	logger, err = zap.NewProduction()
	if err != nil {
		panic(fmt.Sprintf("Failed to initialize logger: %v", err))
	}

	prometheus.MustRegister(activeConnections)
	prometheus.MustRegister(commandsExecuted)

	loadConfig()
	initKubernetesClient()
	initPasteConfig() // 初始化粘贴配置
}

func loadConfig() {
	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath(".")

	// 设置默认值
	viper.SetDefault("auth.enabled", true) // 默认启用认证

	// 启用环境变量支持
	viper.AutomaticEnv()
	viper.SetEnvPrefix("WEBSSH") // 环境变量前缀WEBSSH_

	// 将配置中的点替换为下划线，以便匹配环境变量
	// 例如：auth.verify_token_url对应环境变量WEBSSH_AUTH_VERIFY_TOKEN_URL
	replacer := strings.NewReplacer(".", "_")
	viper.SetEnvKeyReplacer(replacer)

	// 读取配置文件
	if err := viper.ReadInConfig(); err != nil {
		logger.Warn("Error reading config file, will use environment variables or defaults", zap.Error(err))
	}

	// 打印配置信息
	verifyURL := viper.GetString("auth.verify_token_url")
	authEnabled := viper.GetBool("auth.enabled")
	logger.Info("Configuration loaded",
		zap.String("server.address", viper.GetString("server.address")),
		zap.String("auth.verify_token_url", verifyURL),
		zap.Bool("auth.enabled", authEnabled))
}

func initKubernetesClient() {
	var err error
	config, err = loadKubeConfig()
	if err != nil {
		logger.Fatal("Failed to load kubeconfig", zap.Error(err))
	}

	clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		logger.Fatal("Failed to create Kubernetes client", zap.Error(err))
	}
}

func loadKubeConfig() (*rest.Config, error) {
	kubeconfig := viper.GetString("kubernetes.config")
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}

// 验证token的函数
// 支持三种验证端响应格式:
// 1. 直接返回字符串 "true" 或 "false"
// 2. 返回JSON格式 {"success": true/false}
// 3. 根据HTTP状态码判断 (200=成功, 其他=失败)
func verifyToken(token string, namespace string) (bool, error) {
	verifyURL := viper.GetString("auth.verify_token_url")
	timeout := viper.GetInt("auth.timeout")

	reqURL := fmt.Sprintf("%s?namespace=%s", verifyURL, namespace)

	// 打印请求信息
	fmt.Printf("验证请求: URL=%s, namespace=%s\n", reqURL, namespace)

	req, err := http.NewRequest("POST", reqURL, nil)
	if err != nil {
		return false, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-token", token)
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))

	client := &http.Client{
		Timeout: time.Duration(timeout) * time.Second,
	}

	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("请求失败: %v\n", err)
		return false, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	fmt.Printf("响应状态码: %d\n响应内容: %s\n", resp.StatusCode, string(body))

	// 方式1: 验证端直接返回 true 或 false
	bodyStr := strings.TrimSpace(string(body))
	if bodyStr == "true" {
		return true, nil
	} else if bodyStr == "false" {
		return false, nil
	}

	// 方式2: 验证端返回 JSON 格式的 {"success": true/false}
	var result struct {
		Success bool `json:"success"`
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&result); err == nil {
		return result.Success, nil
	}

	// 方式3: 根据HTTP状态码判断 (200 = 成功, 其他 = 失败)
	if resp.StatusCode == 200 {
		return true, nil
	}

	// 默认返回false
	return false, nil
}

func buildShellCommand(hasBash bool) []string {
	if hasBash {
		return []string{
			"bash",
			"-c",
			`export TERM=xterm-256color && 
             export LANG=en_US.UTF-8 && 
             export PS1='\[\033[01;32m\]\u@\h\[\033[00m\]:\[\033[01;34m\]\w\[\033[00m\]\$ ' && 
             export PATH=$PATH:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin &&
             stty icanon &&
             stty iexten &&
             stty -ixon &&
             stty -ixoff &&
             stty echo &&
             set -o emacs &&      # 启用行编辑功能
             if [ -f /etc/bash_completion ]; then
                 . /etc/bash_completion
             fi &&
             bind 'set show-all-if-ambiguous on' &&
             bind 'set completion-ignore-case on' &&
             bind 'set mark-symlinked-directories on' &&
             bind 'set page-completions off' &&
             bind 'set completion-query-items 200' &&
             bind 'set echo-control-characters off' &&
             bind 'set show-all-if-unmodified on' &&
             bind 'set colored-completion-prefix on' &&
             bind 'set colored-stats on' &&
             bind 'set visible-stats on' &&
             bind '"\t": menu-complete' &&
             bind '"\e[Z": menu-complete-backward' &&
             bind '"\C-i": menu-complete' &&
             alias ls='ls --color=auto' &&
             alias ll='ls -l' &&
             alias grep='grep --color=auto' &&
             shopt -s histappend &&
             shopt -s checkwinsize &&
             shopt -s dirspell &&
             shopt -s no_empty_cmd_completion &&
             shopt -s cmdhist &&
             shopt -s lithist &&
             shopt -s direxpand &&
             shopt -s cdspell &&
             export HISTCONTROL=ignoreboth &&
             export HISTSIZE=1000 &&
             bind '"\e[A": history-search-backward' &&
             bind '"\e[B": history-search-forward' &&
             exec bash --norc`,
		}
	}
	return []string{
		"/bin/sh",
		"-c",
		`export TERM=xterm-256color && 
         export PS1='$ ' && 
         set -o vi &&
         stty icanon &&
         stty echo &&
         exec /bin/sh`,
	}
}

func checkBashAvailable(ctx context.Context, clientset *kubernetes.Clientset, namespace, podName, containerName string) bool {
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: containerName,
			Command:   []string{"which", "bash"},
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	var stdout bytes.Buffer
	exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		return false
	}

	if err := exec.Stream(remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: io.Discard,
	}); err != nil {
		return false
	}

	return stdout.String() != ""
}

func main() {
	defer logger.Sync()

	http.HandleFunc("/ws", handleWebSocket)
	http.HandleFunc("/healthz", handleHealthCheck)
	http.Handle("/metrics", promhttp.Handler())

	server := &http.Server{
		Addr:    viper.GetString("server.address"),
		Handler: nil,
	}

	go func() {
		logger.Info("WebSocket server starting", zap.String("address", server.Addr))
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("ListenAndServe failed", zap.Error(err))
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	logger.Info("Shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		logger.Fatal("Server forced to shutdown", zap.Error(err))
	}

	logger.Info("Server exiting")
}

func handleHealthCheck(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// 检查是否启用认证
	authEnabled := viper.GetBool("auth.enabled")

	// 获取namespace和容器信息
	namespace := r.URL.Query().Get("namespace")
	podName := r.URL.Query().Get("podName")
	containerName := r.URL.Query().Get("containerName")

	// 即使禁用认证，这些参数也是必需的
	if namespace == "" || podName == "" || containerName == "" {
		logger.Error("Missing resource information",
			zap.String("namespace", namespace),
			zap.String("podName", podName),
			zap.String("containerName", containerName))
		http.Error(w, "Missing required parameters", http.StatusBadRequest)
		return
	}

	// 认证流程，仅在启用认证时执行
	if authEnabled {
		// 获取认证信息
		xToken := r.URL.Query().Get("x-token")

		if xToken == "" {
			logger.Error("Missing token")
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// 验证token和资源访问权限
		valid, err := verifyToken(xToken, namespace)
		if err != nil {
			logger.Error("Token verification failed",
				zap.Error(err),
				zap.String("namespace", namespace))
			http.Error(w, "Verification failed", http.StatusUnauthorized)
			return
		}

		if !valid {
			logger.Error("Invalid token or unauthorized namespace access",
				zap.String("namespace", namespace))
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
	} else {
		logger.Warn("Authentication disabled, proceeding without verification",
			zap.String("namespace", namespace),
			zap.String("podName", podName))
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		logger.Error("Failed to upgrade connection", zap.Error(err))
		return
	}
	defer conn.Close()

	activeConnections.Inc()
	defer activeConnections.Dec()

	logger.Info("WebSocket connection established", zap.String("remoteAddr", conn.RemoteAddr().String()))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stdinReader, stdinWriter := io.Pipe()
	defer stdinWriter.Close()

	terminalSession := &TerminalSession{
		conn:        conn,
		stdinReader: stdinReader,
		sizeChan:    make(chan remotecommand.TerminalSize),
		done:        make(chan struct{}),
		tty:         true,
		inputBuffer: make(chan []byte, 100), // 添加输入缓冲区
	}
	defer terminalSession.Close()

	terminalSession.initializeEnv()

	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			logger.Error("Failed to read message", zap.Error(err))
			break
		}

		msg := messagePool.Get().(*Message)
		defer messagePool.Put(msg)

		if err := json.Unmarshal(message, msg); err != nil {
			logger.Error("Failed to unmarshal message", zap.Error(err))
			sendError(conn, "Invalid message format")
			continue
		}

		switch msg.Type {
		case "init":
			hasBash := checkBashAvailable(ctx, clientset, msg.Namespace, msg.PodName, msg.ContainerName)
			command := buildShellCommand(hasBash)

			pod, err := clientset.CoreV1().Pods(msg.Namespace).Get(ctx, msg.PodName, metav1.GetOptions{})
			if err != nil {
				logger.Error("Failed to get pod", zap.Error(err))
				sendError(conn, fmt.Sprintf("Failed to get pod: %v", err))
				continue
			}

			if pod.Status.Phase != corev1.PodRunning {
				sendError(conn, fmt.Sprintf("Pod is not running (current status: %s)", pod.Status.Phase))
				continue
			}

			containerExists := false
			for _, container := range pod.Spec.Containers {
				if container.Name == msg.ContainerName {
					containerExists = true
					break
				}
			}
			if !containerExists {
				sendError(conn, fmt.Sprintf("Container %s not found in pod %s", msg.ContainerName, msg.PodName))
				continue
			}

			req := clientset.CoreV1().RESTClient().Post().
				Resource("pods").
				Name(msg.PodName).
				Namespace(msg.Namespace).
				SubResource("exec").
				VersionedParams(&corev1.PodExecOptions{
					Container: msg.ContainerName,
					Command:   command,
					Stdin:     true,
					Stdout:    true,
					Stderr:    true,
					TTY:       true,
				}, scheme.ParameterCodec)

			executor, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
			if err != nil {
				logger.Error("Failed to create executor", zap.Error(err))
				sendError(conn, fmt.Sprintf("Failed to create executor: %v", err))
				continue
			}

			go func() {
				err := executor.Stream(remotecommand.StreamOptions{
					Stdin:             terminalSession,
					Stdout:            terminalSession,
					Stderr:            terminalSession,
					Tty:               true,
					TerminalSizeQueue: terminalSession,
				})

				if err != nil {
					logger.Error("Stream ended with error", zap.Error(err))
					sendError(conn, fmt.Sprintf("Shell ended with error: %v", err))
				}
			}()

		case "input":
			if msg.Data != "" {
				if msg.Data == "\t" {
					// 处理Tab键
					_, err := stdinWriter.Write([]byte{0x09}) // 使用 raw tab 字符
					if err != nil {
						logger.Error("Failed to write tab to stdin", zap.Error(err))
					}
					// 尝试强制刷新
					stdinWriter.Write([]byte{})
				} else {
					// 检查粘贴大小限制
					if len(msg.Data) > maxPasteSize {
						sendError(conn, fmt.Sprintf("粘贴内容过大 (%d 字符)，最大允许 %d 字符", len(msg.Data), maxPasteSize))
						continue
					}

					// 检测是否为长文本，使用分块处理
					if enableConfirm && terminalSession.isLongText(msg.Data) {
						// 发送粘贴确认提示
						dataLen := len(msg.Data)
						lines := strings.Count(msg.Data, "\n") + 1
						confirmMsg := &Message{
							Type:         "paste_confirm",
							Message:      fmt.Sprintf("检测到大文本粘贴 (%d 字符, %d 行)。是否继续？", dataLen, lines),
							IsLargePaste: true,
							Data:         msg.Data, // 暂存数据
						}
						conn.WriteJSON(confirmMsg)
					} else {
						// 根据大小决定处理方式
						if terminalSession.isLongText(msg.Data) {
							go terminalSession.processLongText(msg.Data, stdinWriter)
						} else {
							// 检查是否包含多行或特殊字符
							if strings.Contains(msg.Data, "\n") || terminalSession.needsCleanup(msg.Data) {
								// 使用安全粘贴方式
								cleanData := terminalSession.cleanPasteData(msg.Data)
								terminalSession.writePasteData(stdinWriter, []byte(cleanData))
							} else {
								// 普通单行文本直接写入
								_, err := stdinWriter.Write([]byte(msg.Data))
								if err != nil {
									logger.Error("Failed to write to stdin", zap.Error(err))
								}
							}
						}
					}
				}
			}

		case "paste_confirm":
			// 处理粘贴确认
			if msg.Data != "" {
				go terminalSession.processLongText(msg.Data, stdinWriter)
			}

		case "resize":
			if msg.Cols > 0 && msg.Rows > 0 {
				terminalSession.sizeChan <- remotecommand.TerminalSize{
					Width:  msg.Cols,
					Height: msg.Rows,
				}
			}

		case "ctrl":
			var ctrlSequence byte
			switch msg.CtrlKey {
			case "c":
				ctrlSequence = 3 // Ctrl+C
			case "d":
				ctrlSequence = 4 // Ctrl+D
			case "l":
				ctrlSequence = 12 // Ctrl+L
			case "z":
				ctrlSequence = 26 // Ctrl+Z
			}
			if ctrlSequence > 0 {
				_, err := stdinWriter.Write([]byte{ctrlSequence})
				if err != nil {
					logger.Error("Failed to write control sequence", zap.Error(err))
				}
			}
		}
	}
}

func sendError(conn *websocket.Conn, message string) {
	msg := messagePool.Get().(*Message)
	defer messagePool.Put(msg)

	msg.Type = "error"
	msg.Message = message

	err := conn.WriteJSON(msg)
	if err != nil {
		logger.Error("Failed to send error message", zap.Error(err))
	}
}

func sendInfo(conn *websocket.Conn, message string) {
	msg := messagePool.Get().(*Message)
	defer messagePool.Put(msg)

	msg.Type = "info"
	msg.Message = message

	err := conn.WriteJSON(msg)
	if err != nil {
		logger.Error("Failed to send info message", zap.Error(err))
	}
}
