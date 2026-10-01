package main

import (
	"encoding/json"
	"testing"
)

func TestChatGPTPlanPayload(t *testing.T) {
	input := []byte(`{"model":"gpt-6.1-sol","instructions":"Be useful","input":[{"type":"message","role":"user","content":"hi"},{"type":"function_call","call_id":"call-1","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call-1","output":"done"},{"type":"reasoning","id":"r1","encrypted_content":"opaque"}],"tools":[{"type":"function","name":"lookup","description":"Find it","parameters":{"type":"object"}}],"tool_choice":{"type":"function","name":"lookup"},"store":true,"stream":false,"temperature":0.2,"max_output_tokens":12,"previous_response_id":"old"}`)
	raw, err := toChatGPTPlanPayload(input)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil {
		t.Fatal("invalid payload")
	}
	if out["store"] != false || out["stream"] != true || out["instructions"] != "Be useful" {
		t.Fatal("stateless contract lost")
	}
	for _, key := range []string{"temperature", "max_output_tokens", "previous_response_id"} {
		if _, ok := out[key]; ok {
			t.Fatalf("unsupported field %s forwarded", key)
		}
	}
	tools := out["tools"].([]any)
	ns := tools[0].(map[string]any)
	if len(tools) != 1 || ns["type"] != "namespace" || ns["name"] != "functions" || ns["tools"].([]any)[0].(map[string]any)["name"] != "lookup" {
		t.Fatal("tool not grouped in namespace")
	}
	choice := out["tool_choice"].(map[string]any)
	if choice["type"] != "function" || choice["name"] != "lookup" {
		t.Fatal("forced choice changed")
	}
	items := out["input"].([]any)
	call := items[1].(map[string]any)
	if call["namespace"] != "functions" || call["call_id"] != "call-1" || items[2].(map[string]any)["call_id"] != "call-1" || items[3].(map[string]any)["encrypted_content"] != "opaque" {
		t.Fatal("call/reasoning replay damaged")
	}
}
func TestChatGPTPlanPayloadRejectsAmbiguousToolNames(t *testing.T) {
	for _, body := range []string{`{"input":[],"tools":[{"type":"function","name":"lookup"},{"type":"function","name":"lookup"}]}`, `{"input":[{"type":"message","role":"system","content":"secret"}]}`} {
		if _, err := toChatGPTPlanPayload([]byte(body)); err == nil {
			t.Fatal("ambiguous or unsupported request accepted")
		}
	}
}
