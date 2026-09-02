package config

import "fmt"

// UpsertNotifyChannel adds or replaces (by name) a channel's metadata in the
// config file, atomically at 0600. Credential material is never written
// here — see internal/notify.ChannelKey and cmd/spillway's `notify set`.
func UpsertNotifyChannel(path string, ch NotifyChannelConfig) error {
	cfg, err := readOrDefaults(path)
	if err != nil {
		return err
	}
	for i := range cfg.Notify.Channels {
		if cfg.Notify.Channels[i].Name == ch.Name {
			cfg.Notify.Channels[i] = ch
			return validateAndWrite(path, cfg)
		}
	}
	cfg.Notify.Channels = append(cfg.Notify.Channels, ch)
	return validateAndWrite(path, cfg)
}

// RemoveNotifyChannel deletes a channel by name from the config file.
// Missing is an error — a typo'd remove should be visible, mirroring
// RemoveAccount. The caller is responsible for also deleting the channel's
// secret (issue #101: mirrors `accounts remove` deleting the keychain
// entry).
func RemoveNotifyChannel(path, name string) error {
	cfg, err := readOrDefaults(path)
	if err != nil {
		return err
	}
	for i := range cfg.Notify.Channels {
		if cfg.Notify.Channels[i].Name == name {
			cfg.Notify.Channels = append(cfg.Notify.Channels[:i], cfg.Notify.Channels[i+1:]...)
			return validateAndWrite(path, cfg)
		}
	}
	return fmt.Errorf("notify channel %q not found in %s", name, path)
}

// ListNotifyChannels returns the channels recorded in the config file at
// path, with no daemon required — mirrors ListAccountConfigs.
func ListNotifyChannels(path string) ([]NotifyChannelConfig, error) {
	cfg, err := readOrDefaults(path)
	if err != nil {
		return nil, err
	}
	return cfg.Notify.Channels, nil
}

// FindNotifyChannel returns the named channel's config, or ok=false.
func FindNotifyChannel(path, name string) (NotifyChannelConfig, bool, error) {
	cfg, err := readOrDefaults(path)
	if err != nil {
		return NotifyChannelConfig{}, false, err
	}
	for _, ch := range cfg.Notify.Channels {
		if ch.Name == name {
			return ch, true, nil
		}
	}
	return NotifyChannelConfig{}, false, nil
}
