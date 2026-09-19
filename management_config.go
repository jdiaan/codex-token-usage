package main

import (
	"bytes"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

const managementConfigFileName = "codex-token-usage-management.yaml"
const managementConfigMaxBytes = 64 * 1024

// This snapshot never enters pluginConfig or an API response. Errors contain only
// stable codes: filesystem and YAML parser errors may contain paths or secrets.
type managementConfigSnapshot struct {
	baseURL, key string
	errorCode    string
	missing      []string
}

func (s managementConfigSnapshot) ready() bool {
	return s.errorCode == "" && s.baseURL != "" && s.key != ""
}

func (s managementConfigSnapshot) status() map[string]any {
	urlSource, keySource := "missing", "missing"
	if s.baseURL != "" {
		urlSource = "local_file"
	}
	if s.key != "" {
		keySource = "local_file"
	}
	return map[string]any{
		"url_source": urlSource, "key_source": keySource,
		"missing_fields": append([]string{}, s.missing...), "error_code": s.errorCode,
	}
}

// Only plugin initialization calls initialize. Reconfiguration reuses both
// successful and failed snapshots; status polling does not access the filesystem.
type managementConfigCache struct {
	once  sync.Once
	mu    sync.RWMutex
	value managementConfigSnapshot
}

var globalManagementConfig = &managementConfigCache{
	value: managementConfigFailure("not_initialized"),
}

func (c *managementConfigCache) initialize() {
	c.once.Do(func() {
		value := loadManagementConfig()
		c.mu.Lock()
		c.value = value
		c.mu.Unlock()
	})
}

func (c *managementConfigCache) current() managementConfigSnapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.value
}

func managementConfigFailure(code string) managementConfigSnapshot {
	return managementConfigSnapshot{errorCode: code, missing: []string{"management_url", "management_key"}}
}

func managementConfigPath() (string, error) {
	if path := strings.TrimSpace(os.Getenv("CPA_TOKEN_USAGE_MANAGEMENT_CONFIG_FILE")); path != "" {
		if !filepath.IsAbs(path) {
			return "", errors.New("invalid_path")
		}
		return filepath.Clean(path), nil
	}
	dir, err := cliProxyDataDir()
	if err != nil || !filepath.IsAbs(dir) {
		return "", errors.New("invalid_path")
	}
	return filepath.Join(dir, "secrets", managementConfigFileName), nil
}

func safeManagementConfigMode(mode os.FileMode, goos string) bool {
	return goos == "windows" || mode.Perm()&0037 == 0
}

func loadManagementConfig() managementConfigSnapshot {
	path, err := managementConfigPath()
	if err != nil {
		return managementConfigFailure("invalid_path")
	}
	// Reject directories/devices/FIFOs before opening. Recheck the opened file
	// so permissions and file type are checked on the descriptor actually read.
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return managementConfigFailure("file_missing")
		}
		return managementConfigFailure("file_unreadable")
	}
	if !info.Mode().IsRegular() {
		return managementConfigFailure("invalid_file")
	}
	f, err := os.Open(path)
	if err != nil {
		return managementConfigFailure("file_unreadable")
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return managementConfigFailure("file_unreadable")
	}
	if !info.Mode().IsRegular() {
		return managementConfigFailure("invalid_file")
	}
	if !safeManagementConfigMode(info.Mode(), runtime.GOOS) {
		return managementConfigFailure("unsafe_permissions")
	}
	raw, err := io.ReadAll(io.LimitReader(f, managementConfigMaxBytes+1))
	if err != nil {
		return managementConfigFailure("file_unreadable")
	}
	if len(raw) > managementConfigMaxBytes {
		return managementConfigFailure("invalid_file")
	}
	return parseManagementConfig(raw)
}

func validManagementURL(value string) bool {
	u, err := url.Parse(value)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

func parseManagementConfig(raw []byte) managementConfigSnapshot {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	var doc yaml.Node
	if err := decoder.Decode(&doc); err != nil {
		return managementConfigFailure("invalid_yaml")
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		return managementConfigFailure("invalid_yaml")
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return managementConfigFailure("invalid_yaml")
	}
	values := make(map[string]string, 2)
	items := doc.Content[0].Content
	for i := 0; i < len(items); i += 2 {
		name, value := items[i], items[i+1]
		if name.Kind != yaml.ScalarNode || name.Tag != "!!str" || (name.Value != "management_url" && name.Value != "management_key") {
			return managementConfigFailure("unknown_field")
		}
		if _, exists := values[name.Value]; exists {
			return managementConfigFailure("duplicate_field")
		}
		if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
			return managementConfigFailure("invalid_type")
		}
		values[name.Value] = strings.TrimSpace(value.Value)
	}
	config := managementConfigSnapshot{baseURL: strings.TrimRight(values["management_url"], "/"), key: values["management_key"]}
	for _, name := range []string{"management_url", "management_key"} {
		if values[name] == "" {
			config.missing = append(config.missing, name)
		}
	}
	if len(config.missing) != 0 {
		config.errorCode = "missing_fields"
	} else if !validManagementURL(config.baseURL) {
		config.errorCode = "invalid_url"
	} else if strings.IndexFunc(config.key, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		config.errorCode = "invalid_key"
	}
	return config
}
