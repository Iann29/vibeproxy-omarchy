package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const baseBackendConfig = `port: 8318
host: 127.0.0.1
amp-upstream-url: "https://ampcode.com"
amp-restrict-management-to-localhost: true
remote-management:
  allow-remote: false
  secret-key: ""
auth-dir: "~/.cli-proxy-api"
debug: false
logging-to-file: false
usage-statistics-enabled: true
proxy-url: ""
request-retry: 3
request-timeout: "10m"
quota-exceeded:
  switch-project: true
  switch-preview-model: true
generative-language-api-key: []
`

var oauthProviderKeys = map[string]string{
	"claude":         "claude",
	"codex":          "codex",
	"gemini":         "gemini-cli",
	"github-copilot": "github-copilot",
	"antigravity":    "antigravity",
	"qwen":           "qwen",
}

type Config struct {
	ProxyPort            int             `yaml:"proxy_port"`
	BackendPort          int             `yaml:"backend_port"`
	BinaryPath           string          `yaml:"binary_path"`
	AuthDir              string          `yaml:"auth_dir"`
	EnabledProviders     map[string]bool `yaml:"enabled_providers"`
	VercelGatewayEnabled bool            `yaml:"vercel_gateway_enabled"`
	VercelAPIKey         string          `yaml:"vercel_api_key"`
	Debug                bool            `yaml:"debug"`
}

func DefaultConfig() *Config {
	home, _ := os.UserHomeDir()
	return &Config{
		ProxyPort:            8317,
		BackendPort:          8318,
		BinaryPath:           filepath.Join(home, ".local", "share", "vibeproxy", "cli-proxy-api-plus"),
		AuthDir:              filepath.Join(home, ".cli-proxy-api"),
		EnabledProviders:     make(map[string]bool),
		VercelGatewayEnabled: false,
		VercelAPIKey:         "",
		Debug:                false,
	}
}

func configPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "vibeproxy", "config.yaml")
}

func DataDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "vibeproxy")
}

func Load() (*Config, error) {
	cfg := DefaultConfig()
	path := configPath()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("reading config: %w", err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	if cfg.EnabledProviders == nil {
		cfg.EnabledProviders = make(map[string]bool)
	}

	return cfg, nil
}

func (c *Config) Save() error {
	path := configPath()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}

	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	return os.WriteFile(path, data, 0644)
}

func (c *Config) EnsureDirectories() error {
	dirs := []string{
		filepath.Dir(configPath()),
		DataDir(),
		c.AuthDir,
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("creating directory %s: %w", dir, err)
		}
	}
	return nil
}

func (c *Config) IsProviderEnabled(key string) bool {
	enabled, ok := c.EnabledProviders[key]
	if !ok {
		return true
	}
	return enabled
}

func (c *Config) SetProviderEnabled(key string, enabled bool) {
	c.EnabledProviders[key] = enabled
}

func (c *Config) GetBackendConfigPath() (string, error) {
	baseConfigPath := filepath.Join(DataDir(), "backend-config.yaml")
	if err := os.MkdirAll(filepath.Dir(baseConfigPath), 0755); err != nil {
		return "", err
	}
	if err := os.WriteFile(baseConfigPath, []byte(baseBackendConfig), 0644); err != nil {
		return "", err
	}

	var zaiAPIKeys []string
	entries, err := os.ReadDir(c.AuthDir)
	if err == nil {
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasPrefix(entry.Name(), "zai-") && strings.HasSuffix(entry.Name(), ".json") {
				data, err := os.ReadFile(filepath.Join(c.AuthDir, entry.Name()))
				if err != nil {
					continue
				}
				var authData map[string]interface{}
				if err := json.Unmarshal(data, &authData); err != nil {
					continue
				}
				if apiKey, ok := authData["api_key"].(string); ok {
					zaiAPIKeys = append(zaiAPIKeys, apiKey)
				}
			}
		}
	}

	var disabledProviders []string
	for serviceKey, oauthKey := range oauthProviderKeys {
		if !c.IsProviderEnabled(serviceKey) {
			disabledProviders = append(disabledProviders, oauthKey)
		}
	}
	sort.Strings(disabledProviders)

	if len(zaiAPIKeys) == 0 && len(disabledProviders) == 0 {
		return baseConfigPath, nil
	}

	var additional strings.Builder

	if len(disabledProviders) > 0 {
		additional.WriteString("\n# Provider exclusions (auto-added by VibeProxy)\noauth-excluded-models:\n")
		for _, provider := range disabledProviders {
			additional.WriteString(fmt.Sprintf("  %s:\n    - \"*\"\n", provider))
		}
	}

	if len(zaiAPIKeys) > 0 && c.IsProviderEnabled("zai") {
		additional.WriteString("\n# Z.AI GLM Provider (auto-added by VibeProxy)\nopenai-compatibility:\n")
		additional.WriteString("  - name: \"zai\"\n")
		additional.WriteString("    base-url: \"https://api.z.ai/api/coding/paas/v4\"\n")
		additional.WriteString("    api-key-entries:\n")
		for _, key := range zaiAPIKeys {
			escapedKey := strings.ReplaceAll(key, `\`, `\\`)
			escapedKey = strings.ReplaceAll(escapedKey, `"`, `\"`)
			additional.WriteString(fmt.Sprintf("      - api-key: \"%s\"\n", escapedKey))
		}
		additional.WriteString("    models:\n")
		additional.WriteString("      - name: \"glm-4.7\"\n        alias: \"glm-4.7\"\n")
		additional.WriteString("      - name: \"glm-4-plus\"\n        alias: \"glm-4-plus\"\n")
		additional.WriteString("      - name: \"glm-4-air\"\n        alias: \"glm-4-air\"\n")
		additional.WriteString("      - name: \"glm-4-flash\"\n        alias: \"glm-4-flash\"\n")
	}

	mergedContent := baseBackendConfig + additional.String()
	mergedConfigPath := filepath.Join(c.AuthDir, "merged-config.yaml")
	if err := os.WriteFile(mergedConfigPath, []byte(mergedContent), 0600); err != nil {
		return baseConfigPath, nil
	}

	return mergedConfigPath, nil
}

func PidFilePath() string {
	return filepath.Join(DataDir(), "vibeproxy.pid")
}

func BackendPidFilePath() string {
	return filepath.Join(DataDir(), "backend.pid")
}

func GenerateRandomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
