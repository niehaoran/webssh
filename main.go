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

	// 处理终端输出，确保正确的换行和字符处理
	output := string(p)

	// 确保输出保持原始格式，不进行任何转换
	msg := &Message{
		Type: "output",
		Data: output,
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
	t.conn.Close()
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
             export COLUMNS=$(tput cols) &&
             export LINES=$(tput lines) &&
             stty icanon &&
             stty iexten &&
             stty -ixon &&
             stty -ixoff &&
             stty echo &&
             stty -onlcr &&
             stty -ocrnl &&
             stty opost &&
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
         export COLUMNS=$(tput cols) &&
         export LINES=$(tput lines) &&
         set -o vi &&
         stty icanon &&
         stty echo &&
         stty -onlcr &&
         stty -ocrnl &&
         stty opost &&
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
					_, err := stdinWriter.Write([]byte(msg.Data))
					if err != nil {
						logger.Error("Failed to write to stdin", zap.Error(err))
					}
				}
			}

		case "resize":
			if msg.Cols > 0 && msg.Rows > 0 {
				size := remotecommand.TerminalSize{
					Width:  msg.Cols,
					Height: msg.Rows,
				}

				// 更新会话中的终端大小
				terminalSession.sizeMutex.Lock()
				terminalSession.lastSize = size
				terminalSession.sizeMutex.Unlock()

				// 发送尺寸变化到终端
				select {
				case terminalSession.sizeChan <- size:
				default:
					// 如果 channel 满了，丢弃旧的大小事件
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
