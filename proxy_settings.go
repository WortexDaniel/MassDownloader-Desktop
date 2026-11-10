package main

import (
	"encoding/json"
	"fmt"
	"massdownloader/internal/core"
)

func (a *App) GetProxySettings() core.ProxyConfig {
	config := core.DefaultProxyConfig()
	if a.settings != nil {
		data, err := json.Marshal(a.settings.Get("proxy"))
		if err == nil {
			_ = json.Unmarshal(data, &config)
		}
	}
	return config
}

func (a *App) setProxySettings(config core.ProxyConfig) error {
	config, err := core.NormalizeProxyConfig(config)
	if err != nil {
		return err
	}
	a.dataMu.Lock()
	defer a.dataMu.Unlock()
	previous := a.settings.Get("proxy")
	a.settings.Set("proxy", config)
	if err := a.settings.Save(); err != nil {
		a.settings.Set("proxy", previous)
		return fmt.Errorf("не удалось сохранить настройки прокси")
	}
	if err := core.SetProxyConfig(config); err != nil {
		return err
	}
	a.RefreshMetadata()
	a.feedBuffer.clear()
	a.emit("proxy:changed")
	return nil
}
