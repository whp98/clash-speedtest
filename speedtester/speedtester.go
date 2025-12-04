package speedtester

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/adapter/provider"
	"github.com/metacubex/mihomo/log"
	"gopkg.in/yaml.v3"

	"github.com/metacubex/mihomo/constant"
)

type Config struct {
	ConfigPaths      string
	FilterRegex      string
	BlockRegex       string
	ServerURL        string
	DownloadSize     int
	UploadSize       int
	Timeout          time.Duration
	Concurrent       int
	MaxLatency       time.Duration
	MinDownloadSpeed float64
	MinUploadSpeed   float64
	FastMode         bool
}

type SpeedTester struct {
	config           *Config
	blockedNodes     []string
	blockedNodeCount int
}

func New(config *Config) *SpeedTester {
	if config.Concurrent <= 0 {
		config.Concurrent = 1
	}
	if config.DownloadSize < 0 {
		config.DownloadSize = 100 * 1024 * 1024
	}
	if config.UploadSize < 0 {
		config.UploadSize = 10 * 1024 * 1024
	}
	return &SpeedTester{
		config: config,
	}
}

type CProxy struct {
	constant.Proxy
	Config map[string]any
}

type RawConfig struct {
	Providers map[string]map[string]any `yaml:"proxy-providers"`
	Proxies   []map[string]any          `yaml:"proxies"`
}

func (st *SpeedTester) LoadProxies(stashCompatible bool) (map[string]*CProxy, error) {
	allProxies := make(map[string]*CProxy)
	st.blockedNodes = make([]string, 0)
	st.blockedNodeCount = 0

	for _, configPath := range strings.Split(st.config.ConfigPaths, ",") {
		configPath = strings.TrimSpace(configPath)
		if configPath == "" {
			continue
		}

		var body []byte
		var err error

		// 获取配置内容
		if strings.HasPrefix(configPath, "http") {
			var resp *http.Response
			resp, err = http.Get(configPath)
			if err != nil {
				log.Warnln("Failed to fetch config from %s: %v", configPath, err)
				continue
			}
			defer resp.Body.Close()
			body, err = io.ReadAll(resp.Body)
			if err != nil {
				log.Warnln("Failed to read config from %s: %v", configPath, err)
				continue
			}
		} else {
			body, err = os.ReadFile(configPath)
			if err != nil {
				log.Warnln("Failed to read config file %s: %v", configPath, err)
				continue
			}
		}

		// 解析配置
		rawCfg := &RawConfig{
			Proxies: []map[string]any{},
		}
		if err := yaml.Unmarshal(body, rawCfg); err != nil {
			log.Warnln("Failed to parse config %s: %v", configPath, err)
			continue
		}

		proxies := make(map[string]*CProxy)
		proxiesConfig := rawCfg.Proxies
		providersConfig := rawCfg.Providers

		// 加载直接定义的代理
		for i, config := range proxiesConfig {
			proxy, err := adapter.ParseProxy(config)
			if err != nil {
				log.Debugln("Skip proxy %d in %s: %v", i, configPath, err)
				continue
			}

			// 处理重名
			proxyName := proxy.Name()
			if _, exist := proxies[proxyName]; exist {
				counter := 1
				for {
					newName := fmt.Sprintf("%s-重名%d", proxyName, counter)
					if _, exist := proxies[newName]; !exist {
						proxyName = newName
						break
					}
					counter++
				}
				log.Debugln("Renamed duplicate proxy: %s -> %s", proxy.Name(), proxyName)
			}
			proxies[proxyName] = &CProxy{Proxy: proxy, Config: config}
		}

		// 加载 provider 中的代理
		for name, config := range providersConfig {
			if name == provider.ReservedName {
				log.Warnln("Skip reserved provider name: %s", provider.ReservedName)
				continue
			}

			pd, err := provider.ParseProxyProvider(name, config)
			if err != nil {
				log.Warnln("Failed to parse provider %s: %v", name, err)
				continue
			}

			if err := pd.Initial(); err != nil {
				log.Warnln("Failed to initialize provider %s: %v", name, err)
				continue
			}

			// 获取 provider 的配置
			urlStr, ok := config["url"].(string)
			if !ok {
				log.Warnln("Provider %s has no valid URL", name)
				continue
			}

			resp, err := http.Get(urlStr)
			if err != nil {
				log.Warnln("Failed to fetch provider %s from %s: %v", name, urlStr, err)
				continue
			}

			providerBody, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				log.Warnln("Failed to read provider %s: %v", name, err)
				continue
			}

			pdRawCfg := &RawConfig{
				Proxies: []map[string]any{},
			}
			if err := yaml.Unmarshal(providerBody, pdRawCfg); err != nil {
				log.Warnln("Failed to parse provider %s config: %v", name, err)
				continue
			}

			// 建立 provider 代理配置映射
			pdProxies := make(map[string]map[string]any)
			for _, pdProxy := range pdRawCfg.Proxies {
				if proxyName, ok := pdProxy["name"].(string); ok {
					pdProxies[proxyName] = pdProxy
				}
			}

			// 添加 provider 中的代理
			for _, proxy := range pd.Proxies() {
				proxyName := fmt.Sprintf("[%s] %s", name, proxy.Name())
				if proxyConfig, ok := pdProxies[proxy.Name()]; ok {
					// 处理重名
					finalName := proxyName
					if _, exist := proxies[finalName]; exist {
						counter := 1
						for {
							newName := fmt.Sprintf("%s-重名%d", proxyName, counter)
							if _, exist := proxies[newName]; !exist {
								finalName = newName
								break
							}
							counter++
						}
						log.Debugln("Renamed duplicate proxy: %s -> %s", proxyName, finalName)
					}
					proxies[finalName] = &CProxy{
						Proxy:  proxy,
						Config: proxyConfig,
					}
				} else {
					log.Debugln("No config found for proxy %s in provider %s", proxy.Name(), name)
				}
			}
		}

		// 过滤和合并代理
		for k, p := range proxies {
			// 检查代理类型
			switch p.Type() {
			case constant.Shadowsocks, constant.ShadowsocksR, constant.Snell, constant.Socks5, constant.Http,
				constant.Vmess, constant.Vless, constant.Trojan, constant.Hysteria, constant.Hysteria2,
				constant.WireGuard, constant.Tuic, constant.Ssh, constant.Mieru, constant.AnyTLS:
				// 支持的类型
			default:
				log.Debugln("Skip unsupported proxy type %s: %s", p.Type(), k)
				continue
			}

			// 修复 IPv6 映射地址
			if server, ok := p.Config["server"]; ok {
				p.Config["server"] = convertMappedIPv6ToIPv4(server.(string))
			}

			// Stash 兼容性检查
			if stashCompatible && !isStashCompatible(p) {
				log.Debugln("Skip non-Stash-compatible proxy: %s", k)
				continue
			}

			// 避免重复
			finalName := k
			if _, ok := allProxies[finalName]; ok {
				counter := 1
				for {
					newName := fmt.Sprintf("%s-重名%d", k, counter)
					if _, ok := allProxies[newName]; !ok {
						finalName = newName
						break
					}
					counter++
				}
				log.Debugln("Renamed duplicate proxy across configs: %s -> %s", k, finalName)
			}
			allProxies[finalName] = p
		}
	}

	log.Infoln("Loaded %d proxies from all configs", len(allProxies))

	// 应用过滤规则
	filterRegexp := regexp.MustCompile(st.config.FilterRegex)
	var blockKeywords []string
	if st.config.BlockRegex != "" {
		for _, keyword := range strings.Split(st.config.BlockRegex, "|") {
			keyword = strings.TrimSpace(keyword)
			if keyword != "" {
				blockKeywords = append(blockKeywords, strings.ToLower(keyword))
			}
		}
	}

	filteredProxies := make(map[string]*CProxy)
	for name := range allProxies {
		// 检查黑名单
		shouldBlock := false
		if len(blockKeywords) > 0 {
			lowerName := strings.ToLower(name)
			for _, keyword := range blockKeywords {
				if strings.Contains(lowerName, keyword) {
					shouldBlock = true
					st.blockedNodes = append(st.blockedNodes, name)
					st.blockedNodeCount++
					break
				}
			}
		}

		if shouldBlock {
			continue
		}

		// 检查白名单（过滤正则）
		if filterRegexp.MatchString(name) {
			filteredProxies[name] = allProxies[name]
		}
	}

	log.Infoln("Filtered to %d proxies (blocked: %d)", len(filteredProxies), st.blockedNodeCount)

	// 如果没有加载到任何代理，返回错误
	if len(filteredProxies) == 0 {
		return nil, fmt.Errorf("no valid proxies loaded from configs")
	}

	return filteredProxies, nil
}

func isStashCompatible(proxy *CProxy) bool {
	switch proxy.Type() {
	case constant.Shadowsocks:
		cipher, ok := proxy.Config["cipher"]
		if ok {
			switch cipher {
			case "aes-128-gcm", "aes-192-gcm", "aes-256-gcm",
				"aes-128-cfb", "aes-192-cfb", "aes-256-cfb",
				"aes-128-ctr", "aes-192-ctr", "aes-256-ctr",
				"rc4-md5", "chacha20", "chacha20-ietf", "xchacha20",
				"chacha20-ietf-poly1305", "xchacha20-ietf-poly1305",
				"2022-blake3-aes-128-gcm", "2022-blake3-aes-256-gcm":
			default:
				return false
			}
		}
	case constant.ShadowsocksR:
		if obfs, ok := proxy.Config["obfs"]; ok {
			switch obfs {
			case "plain", "http_simple", "http_post", "random_head",
				"tls1.2_ticket_auth", "tls1.2_ticket_fastauth":
			default:
				return false
			}
		}
		if protocol, ok := proxy.Config["protocol"]; ok {
			switch protocol {
			case "origin", "auth_sha1_v4", "auth_aes128_md5",
				"auth_aes128_sha1", "auth_chain_a", "auth_chain_b":
			default:
				return false
			}
		}
	case constant.Snell:
		if obfsOpts, ok := proxy.Config["obfs-opts"]; ok {
			if obfsOptsMap, ok := obfsOpts.(map[string]any); ok {
				if mode, ok := obfsOptsMap["mode"]; ok {
					switch mode {
					case "http", "tls":
					default:
						return false
					}
				}
			}
		}
	case constant.Socks5, constant.Http:
	case constant.Vmess:
		if cipher, ok := proxy.Config["cipher"]; ok {
			switch cipher {
			case "auto", "aes-128-gcm", "chacha20-poly1305", "none":
			default:
				return false
			}
		}
		if network, ok := proxy.Config["network"]; ok {
			switch network {
			case "ws", "h2", "http", "grpc":
			default:
				return false
			}
		}
	case constant.Vless:
		if flow, ok := proxy.Config["flow"]; ok {
			switch flow {
			case "xtls-rprx-origin", "xtls-rprx-direct", "xtls-rprx-splice", "xtls-rprx-vision":
			default:
				return false
			}
		}
	case constant.Trojan:
		if network, ok := proxy.Config["network"]; ok {
			switch network {
			case "ws", "grpc":
			default:
				return false
			}
		}
	case constant.Hysteria, constant.Hysteria2:
	case constant.WireGuard:
	case constant.Tuic:
	case constant.Ssh:
	default:
		return false
	}
	return true
}

func (st *SpeedTester) TestProxies(proxies map[string]*CProxy, tester func(result *Result)) {
	if st.config.FastMode {
		// 快速模式：并发测试
		threadNum := st.config.Concurrent
		if threadNum <= 0 {
			threadNum = 10 // 默认并发数
		}

		// 使用 channel 控制并发数量
		semaphore := make(chan struct{}, threadNum)
		resultChan := make(chan *Result, len(proxies)) // 缓冲 channel 存储结果
		var wg sync.WaitGroup

		for name, proxy := range proxies {
			wg.Add(1)
			go func(n string, p *CProxy) {
				defer wg.Done()
				semaphore <- struct{}{}        // 获取信号量（进入并发控制）
				defer func() { <-semaphore }() // 释放信号量

				result := st.testProxy(n, p)
				resultChan <- result
			}(name, proxy)
		}

		// 等待所有 goroutine 完成
		go func() {
			wg.Wait()
			close(resultChan)
		}()

		// 读取结果并调用 tester 回调
		for result := range resultChan {
			tester(result)
		}

	} else {
		// 普通模式：串行测试
		for name, proxy := range proxies {
			result := st.testProxy(name, proxy)
			tester(result)
		}
	}

}

type testJob struct {
	name  string
	proxy *CProxy
}

type Result struct {
	ProxyName     string         `json:"proxy_name"`
	ProxyType     string         `json:"proxy_type"`
	ProxyConfig   map[string]any `json:"proxy_config"`
	Proxy         constant.Proxy `json:"-"`
	Latency       time.Duration  `json:"latency"`
	Jitter        time.Duration  `json:"jitter"`
	PacketLoss    float64        `json:"packet_loss"`
	DownloadSize  float64        `json:"download_size"`
	DownloadTime  time.Duration  `json:"download_time"`
	DownloadSpeed float64        `json:"download_speed"`
	UploadSize    float64        `json:"upload_size"`
	UploadTime    time.Duration  `json:"upload_time"`
	UploadSpeed   float64        `json:"upload_speed"`
}

func (r *Result) FormatDownloadSpeed() string {
	return formatSpeed(r.DownloadSpeed)
}

func (r *Result) FormatLatency() string {
	if r.Latency == 0 {
		return "N/A"
	}
	return fmt.Sprintf("%dms", r.Latency.Milliseconds())
}

func (r *Result) FormatJitter() string {
	if r.Jitter == 0 {
		return "N/A"
	}
	return fmt.Sprintf("%dms", r.Jitter.Milliseconds())
}

func (r *Result) FormatPacketLoss() string {
	return fmt.Sprintf("%.1f%%", r.PacketLoss)
}

func (r *Result) FormatUploadSpeed() string {
	return formatSpeed(r.UploadSpeed)
}

func formatSpeed(bytesPerSecond float64) string {
	units := []string{"B/s", "KB/s", "MB/s", "GB/s", "TB/s"}
	unit := 0
	speed := bytesPerSecond
	for speed >= 1024 && unit < len(units)-1 {
		speed /= 1024
		unit++
	}
	return fmt.Sprintf("%.2f%s", speed, units[unit])
}
func (st *SpeedTester) testProxy(name string, proxy *CProxy) *Result {
	result := &Result{
		ProxyName:   name,
		ProxyType:   proxy.Type().String(),
		ProxyConfig: proxy.Config,
		Proxy:       proxy,
	}

	// 尝试创建客户端并发起请求，任何错误都视为失败
	client := st.createClient(proxy, st.config.MaxLatency)
	// 快速连接测试 - 直接请求一个小数据
	url := fmt.Sprintf("%s/__down?bytes=0", st.config.ServerURL)
	if st.config.FastMode {
		url = "https://www.google.com/generate_204"
	}
	start := time.Now()
	resp, err := client.Get(url)
	if err != nil {
		// 🔔 修改点：打印具体的错误原因 (err.Error())
		//fmt.Printf("\n %s %s %s: %s", name, url, "err connection!", err.Error())
		// 连接失败，返回全零结果
		return result
	}
	err = resp.Body.Close()
	if err != nil {
		return nil
	}
	//fmt.Printf("\n %s %s %d", name, url, resp.StatusCode)
	//5xx代码返回失败
	if resp.StatusCode/100 == 5 {
		// HTTP 状态码异常，返回全零结果
		return result
	}
	// 记录基本延迟
	result.Latency = time.Since(start)
	// FastMode 下只测试连通性就返回
	if st.config.FastMode {
		return result
	}
	// 检查延迟是否超限
	if result.Latency > st.config.MaxLatency {
		return result
	}
	// 2. 并发进行下载测试
	var wg sync.WaitGroup
	var totalDownloadBytes, totalUploadBytes int64
	var totalDownloadTime, totalUploadTime time.Duration
	var downloadCount, uploadCount int
	downloadChunkSize := st.config.DownloadSize / st.config.Concurrent
	if downloadChunkSize > 0 {
		downloadResults := make(chan *downloadResult, st.config.Concurrent)
		for i := 0; i < st.config.Concurrent; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				downloadResults <- st.testDownload(proxy, downloadChunkSize, st.config.Timeout)
			}()
		}
		wg.Wait()
		for range st.config.Concurrent {
			if dr := <-downloadResults; dr != nil {
				totalDownloadBytes += dr.bytes
				totalDownloadTime += dr.duration
				downloadCount++
			}
		}
		close(downloadResults)
		if downloadCount > 0 {
			result.DownloadSize = float64(totalDownloadBytes)
			result.DownloadTime = totalDownloadTime / time.Duration(downloadCount)
			result.DownloadSpeed = float64(totalDownloadBytes) / result.DownloadTime.Seconds()
		}
		// 下载速度不达标，返回（此时已有部分数据）
		if result.DownloadSpeed < st.config.MinDownloadSpeed {
			return result
		}
	}
	// 3. 并发进行上传测试
	uploadChunkSize := st.config.UploadSize / st.config.Concurrent
	if uploadChunkSize > 0 {
		uploadResults := make(chan *downloadResult, st.config.Concurrent)
		for i := 0; i < st.config.Concurrent; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				uploadResults <- st.testUpload(proxy, uploadChunkSize, st.config.Timeout)
			}()
		}
		wg.Wait()
		for i := 0; i < st.config.Concurrent; i++ {
			if ur := <-uploadResults; ur != nil {
				totalUploadBytes += ur.bytes
				totalUploadTime += ur.duration
				uploadCount++
			}
		}
		close(uploadResults)
		if uploadCount > 0 {
			result.UploadSize = float64(totalUploadBytes)
			result.UploadTime = totalUploadTime / time.Duration(uploadCount)
			result.UploadSpeed = float64(totalUploadBytes) / result.UploadTime.Seconds()
		}
	}
	return result
}

type latencyResult struct {
	avgLatency time.Duration
	jitter     time.Duration
	packetLoss float64
}

// 可以删除或简化 testLatency 函数，因为不再需要复杂的延迟统计
// 如果其他地方还在用，可以保留但简化实现：
func (st *SpeedTester) testLatency(proxy constant.Proxy, timeout time.Duration) *latencyResult {
	client := st.createClient(proxy, timeout)

	start := time.Now()
	resp, err := client.Get(fmt.Sprintf("%s/__down?bytes=0", st.config.ServerURL))
	if err != nil {
		return &latencyResult{
			avgLatency: 0,
			jitter:     0,
			packetLoss: 100.0,
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return &latencyResult{
			avgLatency: 0,
			jitter:     0,
			packetLoss: 100.0,
		}
	}

	return &latencyResult{
		avgLatency: time.Since(start),
		jitter:     0,
		packetLoss: 0,
	}
}

type downloadResult struct {
	bytes    int64
	duration time.Duration
}

func (st *SpeedTester) testDownload(proxy constant.Proxy, size int, timeout time.Duration) *downloadResult {
	client := st.createClient(proxy, timeout)
	start := time.Now()

	resp, err := client.Get(fmt.Sprintf("%s/__down?bytes=%d", st.config.ServerURL, size))
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	downloadBytes, _ := io.Copy(io.Discard, resp.Body)

	return &downloadResult{
		bytes:    downloadBytes,
		duration: time.Since(start),
	}
}

func (st *SpeedTester) testUpload(proxy constant.Proxy, size int, timeout time.Duration) *downloadResult {
	client := st.createClient(proxy, timeout)
	reader := NewZeroReader(size)

	start := time.Now()
	resp, err := client.Post(
		fmt.Sprintf("%s/__up", st.config.ServerURL),
		"application/octet-stream",
		reader,
	)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	return &downloadResult{
		bytes:    reader.WrittenBytes(),
		duration: time.Since(start),
	}
}

func (st *SpeedTester) createClient(proxy constant.Proxy, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				var u16Port uint16
				if port, err := strconv.ParseUint(port, 10, 16); err == nil {
					u16Port = uint16(port)
				}
				return proxy.DialContext(ctx, &constant.Metadata{
					Host:    host,
					DstPort: u16Port,
				})
			},
		},
	}
}

func calculateLatencyStats(latencies []time.Duration, failedPings int) *latencyResult {
	result := &latencyResult{
		packetLoss: float64(failedPings) / 6.0 * 100,
	}

	if len(latencies) == 0 {
		return result
	}

	// 计算平均延迟
	var total time.Duration
	for _, l := range latencies {
		total += l
	}
	result.avgLatency = total / time.Duration(len(latencies))

	// 计算抖动
	var variance float64
	for _, l := range latencies {
		diff := float64(l - result.avgLatency)
		variance += diff * diff
	}
	variance /= float64(len(latencies))
	result.jitter = time.Duration(math.Sqrt(variance))

	return result
}

func convertMappedIPv6ToIPv4(server string) string {
	ip := net.ParseIP(server)
	if ip == nil {
		return server
	}
	if ipv4 := ip.To4(); ipv4 != nil {
		return ipv4.String()
	}
	return server
}

type IPLocation struct {
	Country     string `json:"country"`
	CountryCode string `json:"countryCode"`
}

func (st *SpeedTester) GetIPLocation(proxy constant.Proxy) (*IPLocation, error) {
	client := st.createClient(proxy, 10*time.Second)
	resp, err := client.Get("http://ip-api.com/json?fields=country,countryCode")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to get location")
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var location IPLocation
	if err := json.Unmarshal(body, &location); err != nil {
		return nil, err
	}
	return &location, nil
}
