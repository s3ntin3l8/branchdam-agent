package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var envVarRe = regexp.MustCompile(`\$\{([^}]+)\}`)

type Config struct {
	ListenAddr string     `yaml:"listenAddr"`
	LogLevel   string     `yaml:"logLevel"`
	HTTP       HTTPConfig `yaml:"http"`
}

type HTTPConfig struct {
	ReadTimeoutSecs  int `yaml:"readTimeoutSecs"`
	WriteTimeoutSecs int `yaml:"writeTimeoutSecs"`
}

func defaultConfig() Config {
	return Config{
		ListenAddr: ":8080",
		LogLevel:   "info",
		HTTP: HTTPConfig{
			ReadTimeoutSecs:  15,
			WriteTimeoutSecs: 15,
		},
	}
}

func Load(path string) (Config, error) {
	cfg := defaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}

	expanded := expandEnv(string(data))

	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}

	return cfg, nil
}

func expandEnv(s string) string {
	return envVarRe.ReplaceAllStringFunc(s, func(match string) string {
		key := strings.TrimSpace(envVarRe.FindStringSubmatch(match)[1])
		if v, ok := os.LookupEnv(key); ok {
			return v
		}
		return match
	})
}
