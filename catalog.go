package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type catalogModel struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Description   string   `json:"tooltip,omitempty"`
	Reasoning     bool     `json:"reasoning,omitempty"`
	ContextWindow int64    `json:"contextWindow,omitempty"`
	MaxTokens     int64    `json:"maxTokens,omitempty"`
	Input         []string `json:"input,omitempty"`
}

type catalogSnapshot struct {
	FetchedAt int64          `json:"fetched_at"`
	Source    string         `json:"source"`
	Default   string         `json:"default,omitempty"`
	Models    []catalogModel `json:"models"`
}

func (s catalogSnapshot) find(model string) (catalogModel, bool) {
	for _, item := range s.Models {
		if item.ID == model {
			return item, true
		}
	}
	return catalogModel{}, false
}

const modelPrefix = "autoclaw/"

func withModelPrefix(id string) string {
	id = strings.TrimSpace(id)
	if id == "" || strings.HasPrefix(id, modelPrefix) {
		return id
	}
	return modelPrefix + id
}

func trimModelPrefix(id string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(id), modelPrefix))
}

func (a *app) snapshotCatalog() catalogSnapshot {
	a.catalogMu.Lock()
	defer a.catalogMu.Unlock()
	return a.catalog
}

func (a *app) setCatalog(snapshot catalogSnapshot) {
	a.catalogMu.Lock()
	a.catalog = snapshot
	a.catalogMu.Unlock()
}

func (a *app) loadCachedCatalog() (catalogSnapshot, bool) {
	var snapshot catalogSnapshot
	if !a.stateStore().readJSON("models.json", &snapshot) || len(snapshot.Models) == 0 {
		return catalogSnapshot{}, false
	}
	snapshot.Source = "cache"
	return snapshot, true
}

func (a *app) saveCachedCatalog(snapshot catalogSnapshot) {
	if err := a.stateStore().writeJSON("models.json", snapshot); err != nil {
		a.log.warn("catalog", "保存模型缓存失败："+err.Error())
	}
}

// verifyCredential checks a token against the model catalogue without changing
// the pool, so an import fails loudly instead of parking a dead account.
func (a *app) verifyCredential(ctx context.Context, account *credential) error {
	cfg := a.currentConfig()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.modelConfigURL(), nil)
	if err != nil {
		return err
	}
	req.Header = a.catalogHeaders(cfg, account)
	resp, err := a.httpClient().Do(req)
	if err != nil {
		return &upstreamFailure{Status: http.StatusBadGateway, Message: "连接 AutoClaw 失败：" + err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := readLimited(resp.Body, 32<<10)
	return classifyFailure(resp.StatusCode, body)
}

func (a *app) refreshCatalog(ctx context.Context) error {
	err := a.refreshCatalogInner(ctx)
	if err != nil {
		a.diag("catalog refresh: %v", err)
	}
	return err
}

func (a *app) refreshCatalogInner(ctx context.Context) error {
	cfg := a.currentConfig()
	account, errCred := a.anyUsableCredential(ctx)
	a.diag("catalog: credential account=%v err=%v", account != nil, errCred)
	if errCred != nil && (account == nil || account.AccessToken == "") {
		return errCred
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.modelConfigURL(), nil)
	if err != nil {
		return err
	}
	req.Header = a.catalogHeaders(cfg, account)
	resp, err := a.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("模型目录请求失败：%w", err)
	}
	defer resp.Body.Close()
	raw, err := readLimited(resp.Body, 8<<20)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized && account != nil {
		if next, errRefresh := a.refreshAccount(ctx, account); errRefresh == nil && next != nil {
			req2, _ := http.NewRequestWithContext(ctx, http.MethodGet, cfg.modelConfigURL(), nil)
			req2.Header = a.catalogHeaders(cfg, next)
			resp2, err2 := a.httpClient().Do(req2)
			if err2 == nil {
				defer resp2.Body.Close()
				if resp2.StatusCode >= 200 && resp2.StatusCode < 300 {
					raw, _ = readLimited(resp2.Body, 8<<20)
					resp = resp2
				}
			}
		}
	}
	if resp.StatusCode != http.StatusOK {
		a.diag("catalog: HTTP %d body=%s", resp.StatusCode, truncateRunes(strings.TrimSpace(string(raw)), 500))
		return fmt.Errorf("模型目录 HTTP %d：%s", resp.StatusCode, truncateRunes(strings.TrimSpace(string(raw)), 200))
	}
	snapshot, err := parseCatalog(raw)
	if err != nil {
		return err
	}
	if len(snapshot.Models) == 0 {
		return fmt.Errorf("模型目录没有模型")
	}
	snapshot.Source = "remote"
	snapshot.FetchedAt = time.Now().Unix()
	a.setCatalog(snapshot)
	a.saveCachedCatalog(snapshot)
	a.log.info("catalog", fmt.Sprintf("模型目录已更新：%d 个模型", len(snapshot.Models)))
	return nil
}

func parseCatalog(raw []byte) (catalogSnapshot, error) {
	var envelope struct {
		Models []catalogModel `json:"models"`
		Data   struct {
			Models []catalogModel `json:"models"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return catalogSnapshot{}, fmt.Errorf("decode model config: %w", err)
	}
	models := envelope.Models
	if len(models) == 0 {
		models = envelope.Data.Models
	}
	snapshot := catalogSnapshot{}
	seen := map[string]bool{}
	for _, item := range models {
		id := strings.TrimSpace(item.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if item.Name == "" {
			item.Name = id
		}
		if len(item.Input) == 0 {
			item.Input = []string{"text"}
		}
		snapshot.Models = append(snapshot.Models, item)
	}
	if len(snapshot.Models) > 0 {
		snapshot.Default = snapshot.Models[0].ID
		for _, item := range snapshot.Models {
			if item.ID == "zai_glm-5.3-flash" {
				snapshot.Default = item.ID
				break
			}
		}
	}
	return snapshot, nil
}

func (a *app) modelSelected(cfg Config, id string) bool {
	if len(cfg.ModelAllowlist) > 0 {
		allowed := false
		for _, candidate := range cfg.ModelAllowlist {
			if strings.TrimSpace(candidate) == id {
				allowed = true
				break
			}
		}
		if !allowed {
			return false
		}
	}
	for _, candidate := range cfg.ModelDenylist {
		if strings.TrimSpace(candidate) == id {
			return false
		}
	}
	return true
}

func (a *app) modelInfoList() []modelInfo {
	cfg := a.currentConfig()
	snapshot := a.snapshotCatalog()
	created := snapshot.FetchedAt
	if created == 0 {
		created = time.Now().Unix()
	}
	out := make([]modelInfo, 0, len(snapshot.Models))
	for _, item := range snapshot.Models {
		if !a.modelSelected(cfg, item.ID) {
			continue
		}
		input := []string{"text"}
		for _, modality := range item.Input {
			if strings.EqualFold(strings.TrimSpace(modality), "image") {
				input = []string{"text", "image"}
				break
			}
		}
		out = append(out, modelInfo{
			ID:                         withModelPrefix(item.ID),
			Object:                     "model",
			Created:                    created,
			OwnedBy:                    providerKey,
			Type:                       "chat",
			DisplayName:                item.Name,
			Name:                       withModelPrefix(item.ID),
			Description:                item.Description,
			InputTokenLimit:            item.ContextWindow,
			OutputTokenLimit:           item.MaxTokens,
			ContextLength:              item.ContextWindow,
			MaxCompletionTokens:        item.MaxTokens,
			SupportedGenerationMethods: []string{formatChat},
			SupportedParameters: []string{
				"messages", "stream", "max_tokens", "temperature", "top_p", "stop",
				"tools", "tool_choice", "reasoning_effort",
			},
			SupportedInputModalities:  input,
			SupportedOutputModalities: []string{"text"},
		})
	}
	return out
}

func (a *app) registeredModels() []modelInfo {
	models := a.modelInfoList()
	if len(models) > 0 {
		return models
	}
	if cached, ok := a.loadCachedCatalog(); ok {
		a.setCatalog(cached)
		return a.modelInfoList()
	}
	return models
}

func (a *app) modelMaxTokens(model string) int64 {
	if item, ok := a.snapshotCatalog().find(model); ok && item.MaxTokens > 0 {
		return item.MaxTokens
	}
	return 0
}

func (a *app) resolveModel(requested string) (string, error) {
	cfg := a.currentConfig()
	model := trimModelPrefix(requested)
	snapshot := a.snapshotCatalog()
	if len(snapshot.Models) == 0 {
		if model == "" {
			return defaultModel, nil
		}
		return model, nil
	}
	if model == "" {
		model = snapshot.Default
	}
	if model == "" {
		model = snapshot.Models[0].ID
	}
	if _, ok := snapshot.find(model); !ok {
		return "", fmt.Errorf("模型 %q 不在 AutoClaw 目录里；可用：%s", model, strings.Join(a.modelIDs(), ", "))
	}
	if !a.modelSelected(cfg, model) {
		return "", fmt.Errorf("模型 %q 已被插件配置禁用", model)
	}
	return model, nil
}

func (a *app) modelIDs() []string {
	snapshot := a.snapshotCatalog()
	ids := make([]string, 0, len(snapshot.Models))
	for _, item := range snapshot.Models {
		ids = append(ids, withModelPrefix(item.ID))
	}
	return ids
}

func (a *app) catalogLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(a.currentConfig().refreshInterval()):
		}
		refreshCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		if err := a.refreshCatalog(refreshCtx); err != nil {
			a.log.warn("catalog", "模型目录刷新失败："+err.Error())
		}
		cancel()
	}
}
