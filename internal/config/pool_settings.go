package config

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

// PoolFailover is the only pool type so far: a new session goes to the first
// healthy member in pool order. An absent type means failover.
const PoolFailover = "failover"

// PoolBalance spreads new sessions over the pool's connections; a bound
// session keeps its member, as in a failover pool.
const PoolBalance = "balance"

// Pool behavior is persisted independently of the legacy env defaults.
type PoolSettings struct {
	Type          string `json:"type,omitempty"`
	Failover      bool   `json:"failover"`
	FirstByteSec  int    `json:"first_byte_seconds"`
	ProbeSec      int    `json:"probe_seconds"`
	MaxInputChars int    `json:"max_input_chars"`
}

// UnmarshalJSON accepts files written before the pool type existed: their
// numeric "balance" no longer does anything and is dropped at the next write.
func (s *PoolSettings) UnmarshalJSON(data []byte) error {
	type plain PoolSettings
	var in struct {
		plain
		Balance *json.RawMessage `json:"balance"`
	}
	if err := json.Unmarshal(data, &in); err != nil {
		return err
	}
	*s = PoolSettings(in.plain)
	if in.Balance != nil {
		log.Printf("pool_settings: поле balance устарело и не используется; тип пула задаёт поле type")
	}
	return nil
}

func (c Config) PoolSettings(name string) PoolSettings {
	if s, ok := c.Local.PoolSettings[name]; ok {
		return s
	}
	return PoolSettings{Failover: c.Failover, FirstByteSec: c.FirstByteSec(), ProbeSec: c.ProbeSec(), MaxInputChars: c.MaxInputChars}
}

func (s PoolSettings) Apply(c Config) Config {
	c.Failover = s.Failover
	c.FirstByte = time.Duration(s.FirstByteSec) * time.Second
	c.ProbeEvery = time.Duration(s.ProbeSec) * time.Second
	c.MaxInputChars = s.MaxInputChars
	return c
}

func (s PoolSettings) validate() error {
	if s.Type != "" && s.Type != PoolFailover && s.Type != PoolBalance {
		return fmt.Errorf("тип пула %q не поддерживается (есть failover и balance)", s.Type)
	}
	if s.FirstByteSec < 0 || s.ProbeSec < 0 || s.MaxInputChars < 0 {
		return fmt.Errorf("настройки пула: значения должны быть целыми числами >= 0")
	}
	// Probe contexts use twice the interval; reject duration overflow.
	const maxSeconds = int64((1<<63 - 1) / (2 * time.Second))
	if int64(s.FirstByteSec) > maxSeconds || int64(s.ProbeSec) > maxSeconds {
		return fmt.Errorf("слишком большой таймаут или интервал проверки")
	}
	return nil
}

func ParsePoolSettings(in SettingsInput) (PoolSettings, error) {
	s := PoolSettings{Type: strings.TrimSpace(in.Type), Failover: in.Failover == "1"}
	for _, field := range []struct {
		name, value string
		out         *int
	}{
		{"Ожидание начала ответа", in.FirstByte, &s.FirstByteSec},
		{"Интервал проверки", in.ProbeEvery, &s.ProbeSec},
		{"Лимит контекста", in.MaxInputChars, &s.MaxInputChars},
	} {
		n, err := strconv.Atoi(strings.TrimSpace(field.value))
		if err != nil || n < 0 {
			return s, fmt.Errorf("%s: нужно целое число >= 0", field.name)
		}
		*field.out = n
	}
	return s, s.validate()
}

// Copy old global values once, preserving explicitly configured zero/false.
func MigratePoolSettings(c Config) (Local, bool) {
	l := c.Local.Clone()
	changed := false
	if l.PoolSettings == nil {
		l.PoolSettings = map[string]PoolSettings{}
	}
	for name := range l.ModelPools {
		if _, ok := l.PoolSettings[name]; !ok {
			l.PoolSettings[name] = c.PoolSettings(name)
			changed = true
		}
	}
	return l, changed
}

// SavePoolSettings stores settings as given; an empty Type keeps the stored one.
func (s *Store) SavePoolSettings(name string, settings PoolSettings, expectedProfile ...string) error {
	return s.UpdatePoolSettings(name, func(old PoolSettings) PoolSettings {
		if settings.Type == "" {
			settings.Type = old.Type
		}
		return settings
	}, expectedProfile...)
}

// UpdatePoolSettings merges into the latest snapshot under the store lock, so
// a simultaneous catalog refresh or another pool's settings update cannot be
// overwritten, and a form never overwrites a field it does not show with a
// stale value.
func (s *Store) UpdatePoolSettings(name string, merge func(old PoolSettings) PoolSettings, expectedProfile ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(expectedProfile) > 0 && expectedProfile[0] != "" && expectedProfile[0] != s.c.Local.ActiveProfile {
		return fmt.Errorf("активный профиль изменился; обновите страницу")
	}
	if _, ok := s.c.Local.ModelPools[name]; !ok {
		return fmt.Errorf("пул %q не найден", name)
	}
	l, _ := MigratePoolSettings(s.c)
	settings := merge(l.PoolSettings[name])
	if err := settings.validate(); err != nil {
		return err
	}
	l.PoolSettings[name] = settings
	if s.provPath == "" {
		return fmt.Errorf("файл настроек провайдеров отключён")
	}
	if err := l.syncActiveProfile(); err != nil {
		return err
	}
	if err := WriteProviders(s.provPath, l); err != nil {
		return err
	}
	s.wroteProviders()
	s.c.Local = l
	return nil
}
