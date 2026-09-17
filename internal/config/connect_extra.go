package config

// extraConnectCatalog is curated in-tree (not from models.dev).
// gen_connect.go overwrites connect_catalog.go; keep extra rows here so a
// regen cannot drop them. lookupConnect prefers this map.
var extraConnectCatalog = map[string]connectEntry{
	"xdev-server": {
		Title:   "xdev-server",
		BaseURL: "${XDEV_SERVER_URL}/v1",
		API:     "openai-completions",
		Env:     []string{"XDEV_SERVER_KEY"},
		Doc:     "https://github.com/FreePeak/onegw",
		Models: []connectModel{
			{ID: "free", Name: "Free", ContextWindow: 200000, MaxTokens: 8192, Reasoning: true},
			{ID: "xdev", Name: "xdev", ContextWindow: 200000, MaxTokens: 8192, Reasoning: true},
			{ID: "deepseek-v4.1-flash", Name: "DeepSeek V4.1 Flash", ContextWindow: 200000, MaxTokens: 8192, Reasoning: true},
			{ID: "qwen3.8-flash", Name: "Qwen3.8 Flash", ContextWindow: 200000, MaxTokens: 8192},
			{ID: "glm-5.3-flash", Name: "GLM-5.3 Flash", ContextWindow: 200000, MaxTokens: 8192, Reasoning: true},
			{ID: "mimo-v2.5", Name: "Mimo v2.5", ContextWindow: 200000, MaxTokens: 8192},
		},
	},
}

func lookupConnect(name string) (connectEntry, bool) {
	if e, ok := extraConnectCatalog[name]; ok {
		return e, true
	}
	e, ok := connectCatalog[name]
	return e, ok
}
