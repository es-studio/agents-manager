package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var toolPartTypes = map[string]bool{
	"tool_use":               true,
	"tool_result":            true,
	"tool_use_input":         true,
	"function_call":          true,
	"function_call_output":   true,
	"server_tool_use":        true,
	"web_search_tool_result": true,
}

var consecutiveNewlines = regexp.MustCompile(`\n{3,}`)

func readableText(value any) string {
	if value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		return consecutiveNewlines.ReplaceAllString(strings.TrimSpace(s), "\n\n")
	}
	if l, ok := value.([]any); ok {
		var parts []string
		for _, v := range l {
			if rText := readableText(v); rText != "" {
				parts = append(parts, rText)
			}
		}
		return strings.Join(parts, "\n")
	}
	if m, ok := value.(map[string]any); ok {
		if typ, ok := m["type"].(string); ok && toolPartTypes[typ] {
			switch typ {
			case "tool_use":
				name, _ := m["name"].(string)
				var inputStr string
				if inputVal, ok := m["input"]; ok {
					if inputMap, ok := inputVal.(map[string]any); ok {
						if cmd, ok := inputMap["command"].(string); ok {
							inputStr = "command: " + cmd
						} else {
							data, err := json.MarshalIndent(inputMap, "", "  ")
							if err == nil {
								inputStr = string(data)
							}
						}
					} else if inputS, ok := inputVal.(string); ok {
						inputStr = inputS
					}
				}
				if name != "" {
					return fmt.Sprintf("[TOOL_USE: %s]\n%s", name, strings.TrimSpace(inputStr))
				}
				return fmt.Sprintf("[TOOL_USE]\n%s", strings.TrimSpace(inputStr))
			case "tool_result":
				var contentStr string
				toolUseID, _ := m["tool_use_id"].(string)
				toolName := currentSessionTools[toolUseID]
				bashCmd := currentSessionCommands[toolUseID]

				isRead := false
				if toolName != "" {
					nameLower := strings.ToLower(toolName)
					if strings.Contains(nameLower, "view") || strings.Contains(nameLower, "read") || strings.Contains(nameLower, "show") {
						isRead = true
					}
					if nameLower == "bash" && bashCmd != "" {
						cmdLower := strings.ToLower(bashCmd)
						words := strings.Fields(cmdLower)
						for _, w := range words {
							if w == "cat" || w == "grep" || w == "tail" || w == "head" || w == "less" || w == "more" {
								isRead = true
								break
							}
						}
					}
				}

				if isRead {
					contentStr = "(READ CODE OMITTED)"
				} else if contentVal, ok := m["content"]; ok {
					if contentS, ok := contentVal.(string); ok {
						contentStr = contentS
					} else if contentL, ok := contentVal.([]any); ok {
						var blockTexts []string
						for _, item := range contentL {
							if itemMap, ok := item.(map[string]any); ok {
								if text, ok := itemMap["text"].(string); ok {
									blockTexts = append(blockTexts, text)
								} else {
									data, _ := json.Marshal(itemMap)
									blockTexts = append(blockTexts, string(data))
								}
							} else if itemS, ok := item.(string); ok {
								blockTexts = append(blockTexts, itemS)
							}
						}
						contentStr = strings.Join(blockTexts, "\n")
					} else {
						data, err := json.Marshal(contentVal)
						if err == nil {
							contentStr = string(data)
						}
					}
				}
				isErr, _ := m["is_error"].(bool)
				errStr := ""
				if isErr {
					errStr = " (ERROR)"
				}
				return fmt.Sprintf("[TOOL_RESULT]%s\n%s", errStr, strings.TrimSpace(contentStr))
			default:
				var detail string
				for _, k := range []string{"content", "text", "message", "command", "arguments"} {
					if val, ok := m[k]; ok {
						detail = readableText(val)
						if detail != "" {
							break
						}
					}
				}
				if detail == "" {
					data, _ := json.Marshal(m)
					detail = string(data)
				}
				return fmt.Sprintf("[%s]\n%s", strings.ToUpper(typ), strings.TrimSpace(detail))
			}
		}
		keys := []string{"text", "input_text", "content", "message", "lastPrompt"}
		for _, key := range keys {
			if val, ok := m[key]; ok {
				if rText := readableText(val); rText != "" {
					return rText
				}
			}
		}
	}
	return ""
}

func appendRawBlock(blocks *[]string, role string, textVal any) {
	text := readableText(textVal)
	if text == "" {
		return
	}
	block := fmt.Sprintf("\n[%s]\n%s\n", role, strings.TrimSpace(text))
	*blocks = append(*blocks, block)
}

func truncateBlocksFromEnd(blocks []string, maxChars int) string {
	var kept []string
	totalLen := 0
	for i := len(blocks) - 1; i >= 0; i-- {
		b := blocks[i]
		if totalLen+len(b) > maxChars {
			kept = append(kept, "\n[TRUNCATED]\n")
			break
		}
		kept = append(kept, b)
		totalLen += len(b)
	}

	var reversed []string
	for i := len(kept) - 1; i >= 0; i-- {
		reversed = append(reversed, kept[i])
	}
	return strings.TrimSpace(strings.Join(reversed, ""))
}

func appendContextPart(parts *[]string, role string, textVal any, maxChars int) bool {
	text := readableText(textVal)
	if text == "" {
		return true
	}
	block := fmt.Sprintf("\n[%s]\n%s\n", role, strings.TrimSpace(text))
	current := 0
	for _, p := range *parts {
		current += len(p)
	}
	if current+len(block) > maxChars {
		remain := maxChars - current
		if remain > 80 {
			*parts = append(*parts, block[:remain]+"\n\n[TRUNCATED]\n")
		}
		return false
	}
	*parts = append(*parts, block)
	return true
}

const codexEnvContextEndTag = "</environment_context>"

func contextFromCodexJSONL(path string, maxChars int) string {
	bootstrapDone := true
	if data, err := os.ReadFile(path); err == nil && strings.Contains(string(data), codexEnvContextEndTag) {
		bootstrapDone = false
	}

	var blocks []string
	tryAppend := func(role string, text string) {
		if text == "" {
			return
		}
		if !bootstrapDone {
			idx := strings.Index(text, codexEnvContextEndTag)
			if idx < 0 {
				return
			}
			bootstrapDone = true
			text = strings.TrimSpace(text[idx+len(codexEnvContextEndTag):])
			if text == "" {
				return
			}
		}
		appendRawBlock(&blocks, role, text)
	}

	_ = parseJSONLStream(path, func(obj map[string]any) bool {
		typ, _ := obj["type"].(string)
		payload, _ := obj["payload"].(map[string]any)
		if typ == "event_msg" && payload != nil {
			pType, _ := payload["type"].(string)
			if pType == "user_message" {
				tryAppend("USER", readableText(payload["message"]))
			}
		} else if typ == "response_item" && payload != nil {
			pType, _ := payload["type"].(string)
			if pType == "message" {
				role := "MESSAGE"
				if rStr, ok := payload["role"].(string); ok && rStr != "" {
					role = strings.ToUpper(rStr)
				}
				if role == "SYSTEM" {
					return true
				}
				tryAppend(role, readableText(payload["content"]))
			}
		}
		return true
	})
	return truncateBlocksFromEnd(blocks, maxChars)
}

func contextFromClaudeJSONL(path string, maxChars int) string {
	var blocks []string
	_ = parseJSONLStream(path, func(obj map[string]any) bool {
		typ, _ := obj["type"].(string)
		if typ == "user" {
			appendRawBlock(&blocks, "USER", obj["message"])
		} else if typ == "assistant" {
			appendRawBlock(&blocks, "ASSISTANT", obj["message"])
		}
		return true
	})
	return truncateBlocksFromEnd(blocks, maxChars)
}

func contextFromCursorJSONL(path string, maxChars int) string {
	var blocks []string
	_ = parseJSONLStream(path, func(obj map[string]any) bool {
		role, _ := obj["role"].(string)
		if role != "" {
			appendRawBlock(&blocks, strings.ToUpper(role), obj["message"])
		}
		return true
	})
	return truncateBlocksFromEnd(blocks, maxChars)
}



func contextFromAntigravityJSONL(path string, maxChars int) string {
	var blocks []string
	_ = parseJSONLStream(path, func(obj map[string]any) bool {
		typ, _ := obj["type"].(string)
		if typ == "USER_INPUT" {
			content, _ := obj["content"].(string)
			req := extractUserRequest(content)
			if req != "" {
				appendRawBlock(&blocks, "USER", req)
			}
		} else if typ == "PLANNER_RESPONSE" {
			content, _ := obj["content"].(string)
			if content != "" {
				appendRawBlock(&blocks, "ANTIGRAVITY", content)
			}
		}
		return true
	})
	return truncateBlocksFromEnd(blocks, maxChars)
}

var currentSessionTools = make(map[string]string)
var currentSessionCommands = make(map[string]string)

func preScanClaudeTools(path string) {
	_ = parseJSONLStream(path, func(obj map[string]any) bool {
		typ, _ := obj["type"].(string)
		if typ == "assistant" {
			msg, _ := obj["message"].(map[string]any)
			if msg != nil {
				content, _ := msg["content"].([]any)
				for _, item := range content {
					itemMap, _ := item.(map[string]any)
					if itemMap != nil {
						itemType, _ := itemMap["type"].(string)
						if itemType == "tool_use" {
							id, _ := itemMap["id"].(string)
							name, _ := itemMap["name"].(string)
							if id != "" && name != "" {
								currentSessionTools[id] = name
								if name == "Bash" {
									if inputMap, ok := itemMap["input"].(map[string]any); ok {
										if cmd, ok := inputMap["command"].(string); ok {
											currentSessionCommands[id] = cmd
										}
									}
								}
							}
						}
					}
				}
			}
		}
		return true
	})
}

func sessionTranscriptContext(session Session, maxChars int) string {
	currentSessionTools = make(map[string]string)
	currentSessionCommands = make(map[string]string)

	if session.SourcePath == "" {
		return ""
	}
	if _, err := os.Stat(session.SourcePath); err != nil {
		return ""
	}
	ext := filepath.Ext(session.SourcePath)
	if session.Provider == "codex" && ext == ".jsonl" {
		return contextFromCodexJSONL(session.SourcePath, maxChars)
	}
	if session.Provider == "claude" && ext == ".jsonl" {
		preScanClaudeTools(session.SourcePath)
		return contextFromClaudeJSONL(session.SourcePath, maxChars)
	}
	if session.Provider == "cursor" && ext == ".jsonl" {
		return contextFromCursorJSONL(session.SourcePath, maxChars)
	}

	if session.Provider == "antigravity" && ext == ".jsonl" {
		return contextFromAntigravityJSONL(session.SourcePath, maxChars)
	}
	return ""
}

func buildSessionContext(session Session) string {
	project := session.ProjectRoot
	if project == "" {
		project = session.Project
	}
	updatedISO := session.UpdatedAt.UTC().Format("2006-01-02T15:04:05") + "Z"
	header := []string{
		"# Agent Session Context",
		"",
		fmt.Sprintf("Provider: %s", session.Provider),
		fmt.Sprintf("Session ID: %s", session.ID),
		fmt.Sprintf("Updated: %s", updatedISO),
		fmt.Sprintf("Project: %s", project),
		fmt.Sprintf("Title: %s", session.Title),
		fmt.Sprintf("Source: %s", session.SourcePath),
		"",
		"## Transcript",
		"",
	}
	transcript := sessionTranscriptContext(session, 120000)
	if transcript == "" {
		transcript = "(transcript unavailable)"
	}
	return strings.Join(header, "\n") + transcript + "\n"
}
