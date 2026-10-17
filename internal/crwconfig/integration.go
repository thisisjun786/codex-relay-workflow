package crwconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

type IntegrationSettings struct {
	RequiredStatus string `json:"required_status"`
}

func (s IntegrationSettings) Validate() error {
	v := s.RequiredStatus
	if v == "" {
		return nil
	}
	if strings.TrimSpace(v) != v {
		return errors.New("integration.required_status must not have surrounding whitespace")
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return errors.New("integration.required_status must not carry a control character")
		}
	}
	return nil
}

func (f *File) Integration() (IntegrationSettings, error) {
	var settings IntegrationSettings
	raw, ok := f.doc["integration"]
	if !ok {
		return settings, nil
	}
	var section map[string]json.RawMessage
	if err := json.Unmarshal(raw, &section); err != nil || section == nil {
		if err == nil {
			err = fmt.Errorf("not an object")
		}
		return settings, fmt.Errorf("%s: integration must be an object: %v", f.path, err)
	}
	v, exists := section["required_status"]
	if !exists {
		return settings, nil
	}
	if bytes.Equal(bytes.TrimSpace(v), []byte("null")) || json.Unmarshal(v, &settings.RequiredStatus) != nil {
		return IntegrationSettings{}, fmt.Errorf("%s: integration.required_status must be a string", f.path)
	}
	if err := settings.Validate(); err != nil {
		return IntegrationSettings{}, fmt.Errorf("%s: %w", f.path, err)
	}
	return settings, nil
}

func LoadIntegration(getenv func(string) string, flagPath string) (IntegrationSettings, error) {
	file, err := loadOptional(getenv, flagPath)
	if err != nil || file == nil {
		return IntegrationSettings{}, err
	}
	return file.Integration()
}
