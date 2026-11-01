package mail

import (
	"encoding/json"
	"errors"
	"os"
)

// LoadConfig reports false when the file does not exist, which simply leaves password reset by email off.
func LoadConfig(path string) (Config, bool, error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, false, nil
	}
	if err != nil {
		return Config{}, false, err
	}

	var config Config
	if err := json.Unmarshal(content, &config); err != nil {
		return Config{}, false, errors.New("mail config cannot be parsed")
	}
	if err := config.Valid(); err != nil {
		return Config{}, false, err
	}

	return config, true, nil
}
