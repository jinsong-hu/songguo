package config

import "gopkg.in/yaml.v3"

// Parse unmarshals YAML data into a Config, normalizes, validates, and builds
// a Snapshot. It is used by tests that construct configs from YAML fixtures;
// production code assembles Config structs directly from stored service rows.
func Parse(data []byte) (*Snapshot, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return Build(cfg)
}
