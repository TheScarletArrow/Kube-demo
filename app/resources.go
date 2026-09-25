package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Лимиты ресурсов так, как их видит сам контейнер: kubelet превращает
// resources.limits в настройки cgroup. После in-place resize эти числа
// меняются у живого процесса — без рестарта.

type cgroupLimits struct {
	CPU    string `json:"cpu"`    // "500m" или "нет"
	Memory string `json:"memory"` // "64Mi" или "нет"
}

func readFirstLine(path string) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0]), true
}

func readCgroupLimits() cgroupLimits {
	l := cgroupLimits{CPU: "?", Memory: "?"}

	// cgroup v2
	if v, ok := readFirstLine("/sys/fs/cgroup/cpu.max"); ok {
		l.CPU = cpuFromQuota(strings.Fields(v))
	} else if q, ok := readFirstLine("/sys/fs/cgroup/cpu/cpu.cfs_quota_us"); ok { // cgroup v1
		p, _ := readFirstLine("/sys/fs/cgroup/cpu/cpu.cfs_period_us")
		if q == "-1" {
			q = "max"
		}
		l.CPU = cpuFromQuota([]string{q, p})
	}

	if v, ok := readFirstLine("/sys/fs/cgroup/memory.max"); ok {
		l.Memory = memFromBytes(v)
	} else if v, ok := readFirstLine("/sys/fs/cgroup/memory/memory.limit_in_bytes"); ok {
		l.Memory = memFromBytes(v)
	}
	return l
}

func cpuFromQuota(f []string) string {
	if len(f) != 2 || f[0] == "max" {
		return "нет"
	}
	quota, err1 := strconv.ParseFloat(f[0], 64)
	period, err2 := strconv.ParseFloat(f[1], 64)
	if err1 != nil || err2 != nil || period == 0 {
		return "?"
	}
	return fmt.Sprintf("%dm", int(quota/period*1000+0.5))
}

func memFromBytes(v string) string {
	if v == "max" {
		return "нет"
	}
	b, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return "?"
	}
	if b >= 1<<60 { // cgroup v1 пишет «почти бесконечность»
		return "нет"
	}
	return fmt.Sprintf("%dMi", b>>20)
}

// parseMilliCPU разбирает "250m", "1", "1.5" в милликоры.
func parseMilliCPU(s string) (int, error) {
	if m, ok := strings.CutSuffix(s, "m"); ok {
		return strconv.Atoi(m)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	return int(f*1000 + 0.5), nil
}
