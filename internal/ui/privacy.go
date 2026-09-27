package ui

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"unicode/utf8"

	"localrouter/internal/privacy"
)

func mountPrivacy(mux *http.ServeMux, b Backend) {
	p, ok := b.(interface{ PrivacyLab() *privacy.Lab })
	if !ok {
		return
	}
	mux.HandleFunc("GET /api/ui/privacy", func(w http.ResponseWriter, r *http.Request) {
		state := p.PrivacyLab().State()
		var traffic *privacy.RuntimeState
		if live, ok := b.(interface{ PrivacyRuntime() *privacy.Runtime }); ok {
			runtime := live.PrivacyRuntime()
			_, _ = runtime.Snapshot()
			current := runtime.State()
			traffic = &current
			state.TrafficApplied = true
		}
		JSON(w, 200, struct {
			privacy.LabState
			Traffic *privacy.RuntimeState `json:"traffic,omitempty"`
		}{state, traffic})
	})
	mux.HandleFunc("GET /api/ui/privacy/config", func(w http.ResponseWriter, r *http.Request) { JSON(w, 200, p.PrivacyLab().Config()) })
	mux.HandleFunc("GET /api/ui/privacy/rules", func(w http.ResponseWriter, r *http.Request) {
		v, err := p.PrivacyLab().LegacyRules()
		privacyReply(w, v, err)
	})
	mux.HandleFunc("POST /api/ui/privacy/validate", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Rules string `json:"rules"`
		}
		if !privacyDecode(w, r, &in) {
			return
		}
		if len(in.Rules) > 64<<10 {
			privacyReply(w, nil, errors.New("invalid_rules"))
			return
		}
		if _, err := privacy.ParseRules([]byte(in.Rules)); err != nil {
			privacyReply(w, nil, errors.New("invalid_rules"))
			return
		}
		JSON(w, 200, json.RawMessage(in.Rules))
	})
	mux.HandleFunc("POST /api/ui/privacy/config", func(w http.ResponseWriter, r *http.Request) {
		if !b.Writable() {
			Error(w, 503, "Роутер сейчас не активен; настройки не изменены")
			return
		}
		var in struct {
			Revision string          `json:"revision"`
			Config   json.RawMessage `json:"config"`
		}
		if !privacyDecode(w, r, &in) {
			return
		}
		snapshot, err := p.PrivacyLab().SaveConfig(r.Context(), in.Revision, in.Config)
		if err != nil {
			privacyReply(w, nil, err)
			return
		}
		JSON(w, 200, snapshot)
	})
	mux.HandleFunc("POST /api/ui/privacy/preview", func(w http.ResponseWriter, r *http.Request) {
		var in privacy.PreviewInput
		if !privacyDecode(w, r, &in) {
			return
		}
		v, err := p.PrivacyLab().Preview(r.Context(), in)
		privacyReply(w, v, err)
	})
	mux.HandleFunc("POST /api/ui/privacy/restore", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ID    string `json:"id"`
			Input string `json:"input"`
		}
		if !privacyDecode(w, r, &in) {
			return
		}
		v, err := p.PrivacyLab().Restore(r.Context(), in.ID, in.Input)
		privacyReply(w, v, err)
	})
	mux.HandleFunc("POST /api/ui/privacy/clear", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ID string `json:"id"`
		}
		if !privacyDecode(w, r, &in) {
			return
		}
		p.PrivacyLab().Clear(in.ID)
		JSON(w, 200, struct{}{})
	})
}

func privacyReply(w http.ResponseWriter, value any, err error) {
	if err == nil {
		JSON(w, 200, value)
		return
	}
	// Never expose parser errors, paths, dictionary entries or submitted text.
	code, status, message := err.Error(), 400, "Проверка не выполнена. Проверьте входные данные и правила."
	switch code {
	case "busy", "capacity":
		status, message = 429, "Лаборатория занята. Очистите предыдущие проверки или повторите позже."
	case "expired":
		status, message = 410, "Временный словарь удалён или истёк. Выполните маскирование заново."
	case "conflict":
		status, message = 409, "Профили изменены в другом окне. Загрузите актуальную версию перед сохранением."
	case "invalid_profiles":
		message = "Профили не сохранены. Проверьте уникальность ID и назначений, ссылки на профили и JSON правил."
	case "invalid_rules":
		message = "Некорректные правила: проверьте поля, типы, регулярные выражения и псевдонимы."
	case "invalid_input", "invalid_mode":
		message = "Ожидается текст UTF-8 или один Anthropic Messages JSON без повторяющихся ключей."
	case "too_large":
		status, message = 413, "Превышен лимит лаборатории: вход до 256 КиБ, ответ до 1 МиБ. Текст не обрезан."
	case "restore_rejected":
		message = "Восстановление отклонено: проверьте JSON, точное написание и тип псевдонимов. Исправьте ответ и повторите."
	case "mask_rejected":
		message = "Фильтр отклонил вход: неподдерживаемый блок, конфликт значений или неоднозначные данные. Измените пример или правила."
	case "detect_rejected":
		message = "Детектирование не выполнено: неподдерживаемый формат или превышен предел анализа. Измените пример или правила."
	case "roundtrip_rejected":
		message = "Обратная проверка не пройдена. Результат не выдан. Проверьте правила."
	case "rules_missing":
		status, message = 404, "Файл privacy.json рядом с providers.json не найден."
	case "cancelled":
		status, message = 408, "Проверка отменена."
	case "save_failed", "unavailable":
		status, message = 503, "Локальное хранилище недоступно. Настройки не изменены."
	default:
		code = "rejected"
	}
	JSON(w, status, map[string]string{"error": message, "code": code})
}

func privacyDecode(w http.ResponseWriter, r *http.Request, out any) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		Error(w, 415, "Ожидается application/json")
		return false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	if err != nil {
		Error(w, 413, "Слишком большой запрос лаборатории")
		return false
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	valid := utf8.Valid(body) && uniqueJSON(json.NewDecoder(bytes.NewReader(body)), 0) == nil
	if !valid || d.Decode(out) != nil || d.Decode(new(any)) != io.EOF {
		Error(w, 400, "Некорректный JSON, повторяющиеся или неизвестные поля")
		return false
	}
	return true
}

// Unlike encoding/json's last-key-wins behavior, the control plane rejects
// ambiguous JSON. The same rule applies recursively to embedded policy objects.
func uniqueJSON(d *json.Decoder, depth int) error {
	if depth > 64 {
		return errors.New("depth")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	if delim, ok := token.(json.Delim); ok {
		keys := map[string]bool{}
		for d.More() {
			if delim == '{' {
				key, err := d.Token()
				if err != nil {
					return err
				}
				s, ok := key.(string)
				if !ok || keys[s] {
					return errors.New("duplicate")
				}
				keys[s] = true
			}
			if err := uniqueJSON(d, depth+1); err != nil {
				return err
			}
		}
		_, err = d.Token()
	}
	return err
}
