package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"

	"mha-go/internal/domain"
)

func LoadFile(path string) (domain.ClusterSpec, error) {
	var cfg fileSpec

	data, err := os.ReadFile(path)
	if err != nil {
		return domain.ClusterSpec{}, fmt.Errorf("read config %q: %w", path, err)
	}

	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
		err = yaml.Unmarshal(data, &cfg)
	case ".toml":
		err = toml.Unmarshal(data, &cfg)
	case ".json":
		err = json.Unmarshal(data, &cfg)
	default:
		return domain.ClusterSpec{}, fmt.Errorf("unsupported config file extension %q", filepath.Ext(path))
	}
	if err != nil {
		return domain.ClusterSpec{}, fmt.Errorf("decode config %q: %w", path, err)
	}

	spec, err := cfg.toDomain()
	if err != nil {
		return domain.ClusterSpec{}, fmt.Errorf("build cluster spec from %q: %w", path, err)
	}
	return spec, nil
}
