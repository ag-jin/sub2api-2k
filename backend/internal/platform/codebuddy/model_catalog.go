package codebuddy

import (
	_ "embed"
	"encoding/json"
	"sort"
)

// M16 模型中心（吸收自 workbuddy2api internal/upstream/model.json）：
// codebuddy 模型目录种子（上下文长度 / 最大输出 / 来源）。
// 积分倍率与多模态能力标记上游未在本文件披露，待账本(A5)或上游接口补充。

//go:embed model_catalog.json
var modelCatalogJSON []byte

// ModelCatalogEntry 单个模型的目录元数据。
type ModelCatalogEntry struct {
	ContextLength   int64  `json:"context_length"`
	MaxOutputTokens int64  `json:"max_output_tokens"`
	Source          string `json:"source"`
}

// CodeBuddyModelCatalog 返回模型目录（模型名升序）。
func CodeBuddyModelCatalog() (map[string]ModelCatalogEntry, []string, error) {
	var catalog map[string]ModelCatalogEntry
	if err := json.Unmarshal(modelCatalogJSON, &catalog); err != nil {
		return nil, nil, err
	}
	keys := make([]string, 0, len(catalog))
	for k := range catalog {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return catalog, keys, nil
}
