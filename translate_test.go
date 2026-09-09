package main

import (
	"encoding/json"
	"testing"
)

// Апстримная reasoning-модель ломается на системном сообщении, пришедшем после
// начала диалога: рассуждение обнуляется, а иногда ответ пуст целиком. Поэтому
// системную роль в теле диалога транслируем в пользовательскую.
func TestToOpenAIRewritesNonLeadingSystemRole(t *testing.T) {
	req := anthropicRequest{
		System: json.RawMessage(`"Ты исполнитель."`),
		Messages: []anthropicMsg{
			{Role: "user", Content: json.RawMessage(`"Задача"`)},
			{Role: "system", Content: json.RawMessage(`"Напоминание"`)},
		},
	}

	out, err := toOpenAI(req, "coding-large")
	if err != nil {
		t.Fatalf("toOpenAI: %v", err)
	}

	roles := make([]string, 0, len(out.Messages))
	for _, m := range out.Messages {
		roles = append(roles, m.Role)
	}
	if len(roles) != 3 {
		t.Fatalf("сообщений: %d, роли %v", len(roles), roles)
	}
	if roles[0] != "system" {
		t.Errorf("ведущее сообщение должно остаться системным, получено %q", roles[0])
	}
	for i, r := range roles[1:] {
		if r == "system" {
			t.Errorf("сообщение %d осталось системным: роли %v", i+1, roles)
		}
	}
	if last := out.Messages[len(out.Messages)-1]; last.Content != "Напоминание" {
		t.Errorf("текст напоминания потерян: %#v", last.Content)
	}
}
