package main

import (
	"sort"
	"strings"
	"sync"
)

var obsoleteConfigState struct {
	sync.RWMutex
	keys []string
}

var obsoleteConfigNames = map[string]bool{
	"scheduling_mode":                    true,
	"scheduler_session_affinity_enabled": true,
	"session_affinity_enabled":           true,
	"同一个Session优先固定到同一个账号":               true,
	"开启账号保护调度（可能会影响缓存）":                  true,
	"开启账号保护调度":                           true,
	"Free 并发上限":                          true,
	"Plus 并发上限":                          true,
	"K12 并发上限":                           true,
	"Team 并发上限":                          true,
	"Pro 并发上限":                           true,
	"Free 5 分钟 Token 上限":                 true,
	"Plus 5 分钟 Token 上限":                 true,
	"K12 5 分钟 Token 上限":                  true,
	"Team 5 分钟 Token 上限":                 true,
	"Pro 5 分钟 Token 上限":                  true,
	"账号保护 Token 窗口秒数":                    true,
	"账号保护预约超时秒数":                         true,
}

func setObsoleteConfigKeys(raw []byte) {
	values := yamlScalars(string(raw))
	keys := make([]string, 0)
	for key := range values {
		if obsoleteConfigNames[key] || strings.HasPrefix(key, "account_protection_") {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	obsoleteConfigState.Lock()
	obsoleteConfigState.keys = keys
	obsoleteConfigState.Unlock()
}

func obsoleteConfigKeys() []string {
	obsoleteConfigState.RLock()
	defer obsoleteConfigState.RUnlock()
	return append([]string(nil), obsoleteConfigState.keys...)
}
