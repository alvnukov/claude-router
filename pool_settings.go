package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Pool behavior is persisted independently of the legacy env defaults.
type poolSettings struct {
	Failover      bool `json:"failover"`
	FirstByteSec  int  `json:"first_byte_seconds"`
	Balance       int  `json:"balance"`
	ProbeSec      int  `json:"probe_seconds"`
	MaxInputChars int  `json:"max_input_chars"`
}

func (c config) poolSettings(name string) poolSettings {
	if s, ok := c.local.PoolSettings[name]; ok {
		return s
	}
	return poolSettings{c.failover, c.FirstByteSec(), c.balance, c.ProbeSec(), c.maxInputChars}
}

func (s poolSettings) apply(c config) config {
	c.failover = s.Failover
	c.firstByte = time.Duration(s.FirstByteSec) * time.Second
	c.balance = s.Balance
	c.probeEvery = time.Duration(s.ProbeSec) * time.Second
	c.maxInputChars = s.MaxInputChars
	return c
}

func (s poolSettings) validate() error {
	if s.FirstByteSec < 0 || s.ProbeSec < 0 || s.Balance < 0 || s.MaxInputChars < 0 {
		return fmt.Errorf("настройки пула: значения должны быть целыми числами >= 0")
	}
	// Probe contexts use twice the interval; reject duration overflow.
	const maxSeconds = int64((1<<63 - 1) / (2 * time.Second))
	if int64(s.FirstByteSec) > maxSeconds || int64(s.ProbeSec) > maxSeconds {
		return fmt.Errorf("слишком большой таймаут или интервал проверки")
	}
	return nil
}

func parsePoolSettings(in settingsInput) (poolSettings, error) {
	s := poolSettings{Failover: in.Failover == "1"}
	for _, field := range []struct {
		name, value string
		out         *int
	}{
		{"Ожидание начала ответа", in.FirstByte, &s.FirstByteSec},
		{"Распределение сессий", in.Balance, &s.Balance},
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
func migratePoolSettings(c config) (localSetup, bool) {
	l := c.local.clone()
	changed := false
	if l.PoolSettings == nil {
		l.PoolSettings = map[string]poolSettings{}
	}
	for name := range l.ModelPools {
		if _, ok := l.PoolSettings[name]; !ok {
			l.PoolSettings[name] = c.poolSettings(name)
			changed = true
		}
	}
	return l, changed
}

// Merge into the latest snapshot so a simultaneous catalog refresh or another
// pool's settings update cannot be overwritten by this form.
func (s *configStore) savePoolSettings(name string, settings poolSettings) error {
	if err := settings.validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.c.local.ModelPools[name]; !ok {
		return fmt.Errorf("пул %q не найден", name)
	}
	l, _ := migratePoolSettings(s.c)
	l.PoolSettings[name] = settings
	if s.provPath == "" {
		return fmt.Errorf("файл настроек провайдеров отключён")
	}
	if err := writeProviders(s.provPath, l); err != nil {
		return err
	}
	s.provMtime = mtime(s.provPath)
	s.c.local = l
	return nil
}
