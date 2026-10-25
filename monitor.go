package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ---------------- 远端主机实时监控 ----------------

// ServerMetrics 远端主机实时指标快照
type ServerMetrics struct {
	Hostname    string   `json:"hostname"`
	Uptime      string   `json:"uptime"`
	LoadAvg     string   `json:"loadAvg"`
	CPUPercent  string   `json:"cpuPercent"` // 0-100 字符串
	MemUsed     string   `json:"memUsed"`
	MemTotal    string   `json:"memTotal"`
	MemPercent  string   `json:"memPercent"`
	DiskMount   string   `json:"diskMount"`
	DiskUsed    string   `json:"diskUsed"`
	DiskTotal   string   `json:"diskTotal"`
	DiskPercent string   `json:"diskPercent"`
	TopCPU      []string `json:"topCpu"`
	TopMem      []string `json:"topMem"`
}

// 单次 SSH exec 采集全部指标，减少往返
const metricsScript = `hostname; echo '===UPTIME==='; uptime; echo '===CPU===';
(top -bn1 2>/dev/null | grep -Ei '^(%?cpu)' | head -1) || vmstat 1 2 | tail -1; echo '===MEM===';
free -h | grep -i '^mem'; echo '===DISK==='; df -h / | tail -1; echo '===TOPCPU===';
ps -eo pid,comm,%cpu --sort=-%cpu 2>/dev/null | head -6 || ps aux | head -6; echo '===TOPMEM===';
ps -eo pid,comm,%mem --sort=-%mem 2>/dev/null | head -6 || ps aux | head -6`

// GetServerMetrics 采集指定会话主机的 CPU/内存/磁盘/进程指标
func (a *App) GetServerMetrics(sessionID string) (ServerMetrics, error) {
	if a.getSession(sessionID) == nil {
		return ServerMetrics{}, errors.New("会话不存在或已断开")
	}
	ctx := &agentCtx{app: a, agentID: "metrics", sessionID: sessionID}
	out, err := ctx.execCommand(metricsScript, nil, 15*time.Second)
	if err != nil {
		return ServerMetrics{}, err
	}
	return parseMetrics(out), nil
}

// parseMetrics 解析分段输出，缺失项留空字符串（前端显示为 —）
func parseMetrics(out string) ServerMetrics {
	var m ServerMetrics
	sections := map[string]string{}
	cur := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "===") && strings.Contains(line, "===") {
			cur = strings.Trim(line, "= \r")
			continue
		}
		sections[cur] += line + "\n"
	}

	m.Hostname = strings.TrimSpace(sections[""])

	if up := strings.TrimSpace(sections["UPTIME"]); up != "" {
		m.Uptime = up
		if i := strings.Index(up, "load average:"); i >= 0 {
			m.LoadAvg = strings.TrimSpace(up[i+len("load average:"):])
			m.Uptime = strings.TrimSpace(up[:i])
		}
	}

	if cpu := strings.TrimSpace(sections["CPU"]); cpu != "" {
		// 形如 %Cpu(s):  5.9 us,  2.0 sy,  0.0 ni, 91.9 id, ...
		for _, field := range strings.Split(cpu, ",") {
			field = strings.TrimSpace(field)
			if strings.HasSuffix(field, "id") {
				num := strings.TrimSpace(strings.TrimSuffix(field, "id"))
				// 去掉可能的前缀描述（如 "Cpu(s):  91.9"）
				if i := strings.LastIndex(num, " "); i >= 0 {
					num = num[i+1:]
				}
				if idle, err := strconv.ParseFloat(num, 64); err == nil {
					m.CPUPercent = fmt.Sprintf("%.1f", 100-idle)
				}
				break
			}
		}
	}

	if mem := strings.TrimSpace(sections["MEM"]); mem != "" {
		// 形如 Mem:   7.6Gi   2.1Gi   1.2Gi ...
		fields := strings.Fields(mem)
		if len(fields) >= 3 {
			m.MemTotal = fields[1]
			m.MemUsed = fields[2]
			if total, ok := parseSizeGiB(fields[1]); ok && total > 0 {
				if used, ok2 := parseSizeGiB(fields[2]); ok2 {
					m.MemPercent = fmt.Sprintf("%.0f", used/total*100)
				}
			}
		}
	}

	if disk := strings.TrimSpace(sections["DISK"]); disk != "" {
		// 形如 /dev/vda1   40G   12G   26G  32% /
		fields := strings.Fields(disk)
		if len(fields) >= 6 {
			m.DiskTotal = fields[1]
			m.DiskUsed = fields[2]
			m.DiskPercent = strings.TrimSuffix(fields[4], "%")
			m.DiskMount = fields[5]
		}
	}

	m.TopCPU = parseProcList(sections["TOPCPU"])
	m.TopMem = parseProcList(sections["TOPMEM"])
	return m
}

// parseSizeGiB 将 7.6Gi / 512Mi / 40G 等容量字符串换算为 GiB
func parseSizeGiB(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	mult := 1.0
	switch {
	case strings.HasSuffix(s, "Ti") || strings.HasSuffix(s, "T"):
		mult = 1024
		s = strings.TrimRight(s, "Tti")
	case strings.HasSuffix(s, "Gi") || strings.HasSuffix(s, "G"):
		mult = 1
		s = strings.TrimRight(s, "Ggi")
	case strings.HasSuffix(s, "Mi") || strings.HasSuffix(s, "M"):
		mult = 1.0 / 1024
		s = strings.TrimRight(s, "Mmi")
	case strings.HasSuffix(s, "Ki") || strings.HasSuffix(s, "K"):
		mult = 1.0 / 1024 / 1024
		s = strings.TrimRight(s, "Kki")
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, false
	}
	return v * mult, true
}

// parseProcList 提取 ps 输出（去掉表头），每行压缩为 "pid 命令 占用%"
func parseProcList(sec string) []string {
	var out []string
	for i, line := range strings.Split(strings.TrimSpace(sec), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if i == 0 && strings.Contains(line, "PID") {
			continue // 表头
		}
		fields := strings.Fields(line)
		if len(fields) >= 3 {
			out = append(out, fields[0]+"  "+fields[1]+"  "+fields[len(fields)-1]+"%")
		}
		if len(out) >= 5 {
			break
		}
	}
	return out
}
