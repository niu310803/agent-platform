package i18n

// ConversationExportText also covers the non-JSON Markdown response body.
func ConversationExportText(locale, key string) string {
	copy := map[string]activityText{
		"title":     {"Chat", "对话"},
		"question":  {"User question", "用户问题"},
		"answer":    {"Assistant answer", "助手回答"},
		"steer":     {"Additional instruction", "补充指令"},
		"origin":    {"Source: Agent `%s`, Chat `%s`, Run `%s`, Tool `%s`.", "来源：Agent `%s`，Chat `%s`，Run `%s`，Tool `%s`。"},
		"empty":     {"No answer was generated.", "未生成回答正文。"},
		"failed":    {"Run failed; the answer may be incomplete.", "运行失败，已有回答可能不完整。"},
		"cancelled": {"Run cancelled; the answer may be incomplete.", "运行已取消，已有回答可能不完整。"},
		"running":   {"The run was still in progress when exported.", "导出时运行尚未完成。"},
	}
	return copy[key].in(locale)
}
