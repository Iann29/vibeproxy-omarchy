package auth

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vibeproxy/vibeproxy-linux/internal/config"
)

type ServiceType string

const (
	ServiceClaude      ServiceType = "claude"
	ServiceCodex       ServiceType = "codex"
	ServiceCopilot     ServiceType = "github-copilot"
	ServiceGemini      ServiceType = "gemini"
	ServiceQwen        ServiceType = "qwen"
	ServiceAntigravity ServiceType = "antigravity"
	ServiceZai         ServiceType = "zai"
	ServiceCodebuff    ServiceType = "codebuff"
)

var allServiceTypes = []ServiceType{
	ServiceClaude, ServiceCodex, ServiceCopilot,
	ServiceGemini, ServiceQwen, ServiceAntigravity, ServiceZai,
	ServiceCodebuff,
}

func (s ServiceType) DisplayName() string {
	switch s {
	case ServiceClaude:
		return "Claude Code"
	case ServiceCodex:
		return "Codex"
	case ServiceCopilot:
		return "GitHub Copilot"
	case ServiceGemini:
		return "Gemini"
	case ServiceQwen:
		return "Qwen"
	case ServiceAntigravity:
		return "Antigravity"
	case ServiceZai:
		return "Z.AI GLM"
	case ServiceCodebuff:
		return "Codebuff"
	default:
		return string(s)
	}
}

type AuthAccount struct {
	ID       string
	Email    string
	Login    string
	Type     ServiceType
	Expired  *time.Time
	FilePath string
}

func (a *AuthAccount) IsExpired() bool {
	if a.Expired == nil {
		return false
	}
	return a.Expired.Before(time.Now())
}

func (a *AuthAccount) DisplayName() string {
	if a.Email != "" {
		return a.Email
	}
	if a.Login != "" {
		return a.Login
	}
	return a.ID
}

type AuthManager struct {
	authDir string
}

func NewAuthManager(authDir string) *AuthManager {
	return &AuthManager{authDir: authDir}
}

func (m *AuthManager) CheckAuthStatus() map[ServiceType][]*AuthAccount {
	result := make(map[ServiceType][]*AuthAccount)
	for _, st := range allServiceTypes {
		result[st] = nil
	}

	entries, err := os.ReadDir(m.authDir)
	if err != nil {
		log.Printf("[Auth] Error reading auth directory %s: %v", m.authDir, err)
		return result
	}

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		fullPath := filepath.Join(m.authDir, entry.Name())
		data, err := os.ReadFile(fullPath)
		if err != nil {
			continue
		}

		var raw map[string]interface{}
		if err := json.Unmarshal(data, &raw); err != nil {
			continue
		}

		typeStr, ok := raw["type"].(string)
		if !ok {
			continue
		}

		st := ServiceType(strings.ToLower(typeStr))
		valid := false
		for _, t := range allServiceTypes {
			if t == st {
				valid = true
				break
			}
		}
		if !valid {
			continue
		}

		email, _ := raw["email"].(string)
		login, _ := raw["login"].(string)

		var expiredDate *time.Time
		if expStr, ok := raw["expired"].(string); ok && expStr != "" {
			for _, layout := range []string{
				"2006-01-02T15:04:05.999999999Z07:00",
				time.RFC3339,
				"2006-01-02T15:04:05Z",
			} {
				if t, err := time.Parse(layout, expStr); err == nil {
					expiredDate = &t
					break
				}
			}
		}

		account := &AuthAccount{
			ID:       entry.Name(),
			Email:    email,
			Login:    login,
			Type:     st,
			Expired:  expiredDate,
			FilePath: fullPath,
		}
		result[st] = append(result[st], account)
	}

	return result
}

func (m *AuthManager) DeleteAccount(account *AuthAccount) error {
	if err := os.Remove(account.FilePath); err != nil {
		return fmt.Errorf("failed to delete auth file: %w", err)
	}
	log.Printf("[Auth] Deleted auth file: %s", account.FilePath)
	return nil
}

func (m *AuthManager) SaveCodebuffCredentials(email, name, authToken, userID, fingerprintID, fingerprintHash string) error {
	if err := os.MkdirAll(m.authDir, 0700); err != nil {
		return fmt.Errorf("creating auth directory: %w", err)
	}

	// Remove any existing codebuff auth files to avoid duplicates.
	entries, _ := os.ReadDir(m.authDir)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "codebuff-") && strings.HasSuffix(entry.Name(), ".json") {
			os.Remove(filepath.Join(m.authDir, entry.Name()))
		}
	}

	payload := map[string]string{
		"type":             "codebuff",
		"email":            email,
		"login":            name,
		"auth_token":       authToken,
		"codebuff_id":      userID,
		"fingerprint_id":   fingerprintID,
		"fingerprint_hash": fingerprintHash,
		"created":          time.Now().UTC().Format(time.RFC3339),
	}

	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}

	filename := fmt.Sprintf("codebuff-%s.json", config.GenerateRandomHex(4))
	return os.WriteFile(filepath.Join(m.authDir, filename), data, 0600)
}

// GetCodebuffToken reads the Codebuff auth token from the auth directory.
func (m *AuthManager) GetCodebuffToken() string {
	entries, err := os.ReadDir(m.authDir)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		if !strings.HasPrefix(entry.Name(), "codebuff-") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(m.authDir, entry.Name()))
		if err != nil {
			continue
		}
		var raw map[string]interface{}
		if err := json.Unmarshal(data, &raw); err != nil {
			continue
		}
		if raw["type"] == "codebuff" {
			if token, ok := raw["auth_token"].(string); ok && token != "" {
				return token
			}
		}
	}
	return ""
}

func (m *AuthManager) SaveZaiAPIKey(apiKey string) error {
	if err := os.MkdirAll(m.authDir, 0700); err != nil {
		return fmt.Errorf("creating auth directory: %w", err)
	}

	masked := apiKey[:8] + "..." + apiKey[len(apiKey)-4:]
	if len(apiKey) <= 12 {
		masked = "****"
	}

	payload := map[string]string{
		"type":    "zai",
		"email":   masked,
		"api_key": apiKey,
		"created": time.Now().UTC().Format(time.RFC3339),
	}

	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}

	filename := fmt.Sprintf("zai-%s.json", config.GenerateRandomHex(4))
	return os.WriteFile(filepath.Join(m.authDir, filename), data, 0600)
}
