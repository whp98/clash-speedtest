package webserver

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/faceair/clash-speedtest/speedtester"
	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

// Server 表示 Web 服务器
type Server struct {
	authKey string
	port    int
}

// New 创建一个新的 Web 服务器实例
func New(port int) (*Server, error) {
	authKey := os.Getenv("AUTH_KEY")
	if authKey == "" {
		return nil, fmt.Errorf("环境变量 AUTH_KEY 未设置，Web 模式需要设置此变量用于身份验证")
	}

	return &Server{
		authKey: authKey,
		port:    port,
	}, nil
}

// Start 启动 Web 服务器
func (s *Server) Start() error {
	http.HandleFunc("/speedtest", s.handleSpeedTest)
	http.HandleFunc("/health", s.handleHealth)

	addr := fmt.Sprintf(":%d", s.port)
	log.Printf("Web 服务器启动在端口 %d", s.port)
	log.Printf("POST /speedtest - 执行测速（需要 Authorization header）")
	log.Printf("GET  /health - 健康检查")

	return http.ListenAndServe(addr, nil)
}

// handleHealth 处理健康检查请求
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}

// handleSpeedTest 处理测速请求
func (s *Server) handleSpeedTest(w http.ResponseWriter, r *http.Request) {
	// 只接受 POST 请求
	if r.Method != http.MethodPost {
		http.Error(w, "只支持 POST 方法", http.StatusMethodNotAllowed)
		return
	}

	// 验证 Authorization header
	authHeader := r.Header.Get("Authorization")
	if !s.validateAuth(authHeader) {
		http.Error(w, "未授权：无效的 Authorization header", http.StatusUnauthorized)
		return
	}

	// 读取请求体（YAML 配置）
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("读取请求体失败: %v", err), http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	if len(body) == 0 {
		http.Error(w, "请求体不能为空", http.StatusBadRequest)
		return
	}

	// 验证是否为有效的 YAML
	var testConfig map[string]interface{}
	if err := yaml.Unmarshal(body, &testConfig); err != nil {
		http.Error(w, fmt.Sprintf("无效的 YAML 格式: %v", err), http.StatusBadRequest)
		return
	}

	log.Printf("收到测速请求，配置大小: %d 字节", len(body))

	// 执行测速
	resultYAML, err := s.performSpeedTest(body)
	if err != nil {
		log.Printf("测速失败: %v", err)
		http.Error(w, fmt.Sprintf("测速失败: %v", err), http.StatusInternalServerError)
		return
	}

	// 返回结果
	w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(resultYAML)

	log.Printf("测速完成，返回结果大小: %d 字节", len(resultYAML))
}

// validateAuth 验证 Authorization header
func (s *Server) validateAuth(authHeader string) bool {
	// 期望格式: "Bearer <token>"
	if authHeader == "" {
		return false
	}

	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || parts[0] != "Bearer" {
		return false
	}

	return parts[1] == s.authKey
}

// performSpeedTest 执行测速并返回结果 YAML
func (s *Server) performSpeedTest(yamlData []byte) ([]byte, error) {
	// 创建临时文件保存配置
	tmpFile, err := os.CreateTemp("", "speedtest-*.yaml")
	if err != nil {
		return nil, fmt.Errorf("创建临时文件失败: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	if _, err := tmpFile.Write(yamlData); err != nil {
		return nil, fmt.Errorf("写入临时文件失败: %v", err)
	}
	tmpFile.Close()

	// 使用固定的默认参数创建 SpeedTester
	config := &speedtester.Config{
		ConfigPaths:      tmpFile.Name(),
		FilterRegex:      ".+",
		BlockRegex:       "",
		ServerURL:        "https://speed.cloudflare.com",
		DownloadSize:     50 * 1024 * 1024,
		UploadSize:       20 * 1024 * 1024,
		Timeout:          2 * time.Second,
		Concurrent:       100,
		MaxLatency:       5000 * time.Millisecond,
		MinDownloadSpeed: 0,
		MinUploadSpeed:   0,
		FastMode:         true, // 快速模式，仅测试延迟
	}

	tester := speedtester.New(config)

	// 加载代理
	allProxies, err := tester.LoadProxies(false)
	if err != nil {
		return nil, fmt.Errorf("加载代理失败: %v", err)
	}

	if len(allProxies) == 0 {
		return nil, fmt.Errorf("配置中没有找到可用的代理节点")
	}

	log.Printf("加载了 %d 个代理节点，开始测速...", len(allProxies))

	// 执行测速
	results := make([]*speedtester.Result, 0)
	var mu sync.Mutex

	tester.TestProxies(allProxies, func(result *speedtester.Result) {
		mu.Lock()
		results = append(results, result)
		mu.Unlock()
		log.Printf("测试完成: %s - 延迟: %s", result.ProxyName, result.FormatLatency())
	})

	// 过滤和处理结果
	validResults := filterResults(results, config)
	log.Printf("过滤后剩余 %d 个有效节点", len(validResults))

	//if len(validResults) == 0 {
	//	return nil, fmt.Errorf("没有符合条件的节点（延迟 < %v）", config.MaxLatency)
	//}

	// 重命名节点
	renameNodes(validResults, tester, config.Concurrent)

	// 生成输出 YAML
	proxies := make([]map[string]any, 0)
	for _, result := range validResults {
		proxies = append(proxies, result.ProxyConfig)
	}

	outputConfig := &speedtester.RawConfig{
		Proxies: proxies,
	}

	yamlOutput, err := yaml.Marshal(outputConfig)
	if err != nil {
		return nil, fmt.Errorf("生成 YAML 失败: %v", err)
	}

	return yamlOutput, nil
}

// filterResults 过滤测速结果
func filterResults(results []*speedtester.Result, config *speedtester.Config) []*speedtester.Result {
	var validResults []*speedtester.Result

	for _, result := range results {
		// 过滤延迟超过最大值的节点
		if config.MaxLatency > 0 && result.Latency > config.MaxLatency {
			continue
		}

		// 过滤延迟为 0 的节点（测试失败）
		if result.Latency == 0 {
			continue
		}

		validResults = append(validResults, result)
	}

	return validResults
}

// renameNodes 重命名节点
func renameNodes(results []*speedtester.Result, tester *speedtester.SpeedTester, concurrent int) {
	var wg sync.WaitGroup
	semaphore := make(chan struct{}, concurrent)

	for _, result := range results {
		wg.Add(1)
		go func(r *speedtester.Result) {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			location, err := tester.GetIPLocation(r.Proxy)
			countryCode := "UNKNOWN"
			if err == nil && location.CountryCode != "" {
				countryCode = location.CountryCode
			}

			proxyConfig := r.ProxyConfig

			// 生成新名称：国家名|国家代码|国旗|延迟|UUID
			newUUID := uuid.New().String()
			proxyConfig["name"] = fmt.Sprintf("%s|%s|%s|%dms|%s",
				getCountryName(countryCode),
				countryCode,
				getCountryFlag(countryCode),
				r.Latency.Milliseconds(),
				newUUID)
		}(result)
	}

	wg.Wait()
}

// getCountryFlag 获取国家旗帜 emoji
func getCountryFlag(code string) string {
	flags := map[string]string{
		"US": "🇺🇸", "CN": "🇨🇳", "GB": "🇬🇧", "UK": "🇬🇧", "JP": "🇯🇵", "DE": "🇩🇪", "FR": "🇫🇷", "RU": "🇷🇺",
		"SG": "🇸🇬", "HK": "🇭🇰", "TW": "🇹🇼", "KR": "🇰🇷", "CA": "🇨🇦", "AU": "🇦🇺", "NL": "🇳🇱", "IT": "🇮🇹",
		"ES": "🇪🇸", "SE": "🇸🇪", "NO": "🇳🇴", "DK": "🇩🇰", "FI": "🇫🇮", "CH": "🇨🇭", "AT": "🇦🇹", "BE": "🇧🇪",
		"UNKNOWN": "🏳️",
	}
	if flag, exists := flags[strings.ToUpper(code)]; exists {
		return flag
	}
	return "🏳️"
}

// getCountryName 获取国家中文名称
func getCountryName(code string) string {
	names := map[string]string{
		"US": "美国", "CN": "中国", "GB": "英国", "UK": "英国", "JP": "日本", "DE": "德国", "FR": "法国", "RU": "俄罗斯",
		"SG": "新加坡", "HK": "香港", "TW": "台湾", "KR": "韩国", "CA": "加拿大", "AU": "澳大利亚", "NL": "荷兰", "IT": "意大利",
		"ES": "西班牙", "SE": "瑞典", "NO": "挪威", "DK": "丹麦", "FI": "芬兰", "CH": "瑞士", "AT": "奥地利", "BE": "比利时",
		"UNKNOWN": "未知",
	}
	if name, exists := names[strings.ToUpper(code)]; exists {
		return name
	}
	return "未知"
}
