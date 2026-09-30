package privacy

import (
	"encoding/json"
	"errors"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Options supplies machine identity and time without coupling the engine to
// router configuration. Empty fields use the current user's machine.
type Options struct {
	ephemeral      bool
	supportedOnly  bool
	Home, Hostname string
	Now            func() time.Time
	newKey         func() ([]byte, error)
	candidate      func(string, []byte, int) string
}

// Engine is a validated, immutable set of rules and a session store. It is safe
// for concurrent callers. Each successful Mask owns a Request until Close.
type Engine struct {
	rules      *Rules
	opt        Options
	detectors  *detectors
	store      *sessionStore
	words      *wordFilter
	allowPaths []*regexp.Regexp
	counts     counters
}

func Open(routerHome string, rules *Rules, opt Options) (*Engine, error) {
	if rules == nil {
		return nil, errors.New("privacy: nil rules")
	}
	r := *rules
	r.Filters = maps.Clone(rules.Filters)
	if !enabled(r.Filters, "sources") {
		r.Sources = "pass"
	}
	if !enabled(r.Filters, "fields") {
		r.fields = nil
	}
	r.Entries = append([]Entry(nil), rules.Entries...)
	r.Domains = append([]string(nil), rules.Domains...)
	r.Allow = append([]string(nil), rules.Allow...)
	r.AllowPaths = append([]string(nil), rules.AllowPaths...)
	r.Networks = append([]netip.Prefix(nil), rules.Networks...)
	r.blocks = append([]netBlock(nil), rules.blocks...)
	r.Patterns = append([]PatternRule(nil), rules.Patterns...)
	r.patterns = append([]patternDetector(nil), rules.patterns...)
	r.Fields = append([]FieldRule(nil), rules.Fields...)
	if enabled(r.Filters, "fields") {
		r.fields = append([]fieldRule(nil), rules.fields...)
	}
	for i := range r.Entries {
		r.Entries[i].Forms = append([]string(nil), r.Entries[i].Forms...)
	}
	if r.policy == nil {
		r.policy = defaultPolicy
	}
	var err error
	if opt.Home == "" {
		opt.Home, err = os.UserHomeDir()
		if err != nil {
			return nil, err
		}
	}
	if opt.Hostname == "" {
		opt.Hostname, err = os.Hostname()
		if err != nil {
			return nil, err
		}
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	var automatic []Entry
	if enabled(r.Filters, "dictionary") {
		automatic = []Entry{{Kind: KindPerson, Forms: []string{filepath.Base(opt.Home)}}, {Kind: KindHost, Forms: []string{opt.Hostname, strings.Split(opt.Hostname, ".")[0]}}}
	}
	for _, auto := range automatic {
		for _, form := range auto.Forms {
			exists := false
			for _, entry := range r.Entries {
				for _, f := range entry.Forms {
					if strings.EqualFold(f, form) {
						exists = true
					}
				}
			}
			if form != "" && form != "." && form != "/" && !exists {
				r.Entries = append(r.Entries, Entry{Kind: auto.Kind, Forms: []string{form}})
			}
		}
	}
	words, err := loadWords()
	if err != nil {
		return nil, err
	}
	e := &Engine{rules: &r, opt: opt, detectors: newDetectors(&r), store: newSessionStore(routerHome, opt.Now), words: words}
	e.store.rules = &r
	if opt.newKey != nil {
		e.store.newKey = opt.newKey
	}
	for _, path := range r.AllowPaths {
		e.allowPaths = append(e.allowPaths, pathPattern(path))
	}
	all := make(map[string]*sessionData)
	if !opt.ephemeral {
		if _, statErr := os.Lstat(e.store.dir); statErr == nil {
			err = e.store.locked(func() error { var readErr error; all, readErr = e.store.readAll(); return readErr })
		} else if !errors.Is(statErr, os.ErrNotExist) {
			err = statErr
		}
	}
	if err != nil {
		return nil, err
	}
	banned := &pseudonyms{words: words, reserved: make(map[string]bool), endings: r.policy.rules.Endings}
	for _, value := range r.Allow {
		banned.reserved[strings.ToLower(value)] = true
	}
	for _, entry := range r.Entries {
		for _, f := range entry.Forms {
			banned.reserved[strings.ToLower(strings.TrimSuffix(f, "*"))] = true
		}
	}
	issued := make(map[string]mapRecord)
	for _, sd := range all {
		for _, entry := range sd.entries {
			banned.reserved[strings.ToLower(entry.Real)] = true
			issued[strings.ToLower(entry.Pseudo)] = entry
			if explicitPseudonym(&r, entry.Kind, entry.Real) != entry.Pseudo {
				banned.reserved[strings.ToLower(entry.Pseudo)] = true
			}
		}
	}
	for _, entry := range r.Entries {
		if entry.Pseudonym != "" {
			if prior, ok := issued[strings.ToLower(entry.Pseudonym)]; ok &&
				(prior.Kind != entry.Kind || prior.Pseudo != entry.Pseudonym || !entryMatches(entry, prior.Real) || explicitPseudonym(&r, prior.Kind, prior.Real) != prior.Pseudo) {
				return nil, &RejectError{Reason: "entries: pseudonym collision", Path: "entries.pseudonym"}
			}
			if banned.collision(entry.Pseudonym) {
				return nil, &RejectError{Reason: "entries: pseudonym collision", Path: "entries.pseudonym"}
			}
			banned.reserved[strings.ToLower(entry.Pseudonym)] = true
		}
	}
	if !opt.ephemeral {
		if _, err := e.Prune(opt.Now()); err != nil {
			return nil, err
		}
	}
	return e, nil
}
func (e *Engine) allowedPath(path string) bool {
	for _, p := range e.allowPaths {
		if p.MatchString(path) {
			return true
		}
	}
	return false
}
func sessionOf(body []byte) string {
	user, ok := lookupString(body, "metadata", "user_id")
	if !ok {
		return ""
	}
	var m struct {
		Session string `json:"session_id"`
	}
	if json.Unmarshal([]byte(user), &m) != nil || !sessionIDRE.MatchString(m.Session) {
		return ""
	}
	return m.Session
}
func (e *Engine) Mask(body []byte) ([]byte, *Request, error) {
	if err := e.Check(body); err != nil {
		return nil, nil, err
	}
	id := sessionOf(body)
	if e.opt.ephemeral {
		id = ""
	}
	req := &Request{engine: e, secrets: make(map[string]string), spellings: make(map[string]string), stats: Stats{Scope: "request", Masked: make(map[Kind]int), Unmasked: make(map[Kind]int)}}
	var masked []byte
	transform := func(view *sessionView) error {
		ip, err := newIPMapper(subkey(view.session.key, "ip"), e.rules)
		if err != nil {
			return err
		}
		req.ip = ip
		req.stats.Version = prfV2
		m := &mapper{e: e, view: view, req: req}
		m.init()
		// Reserve every detected real value before issuing any pseudonym.
		_, err = rewriteRecordedMode(body, func(text string, f fieldKind) ([]textEdit, error) {
			for _, s := range m.plainSpans(text, f) {
				m.names.reserved[strings.ToLower(s.Value)] = true
				if s.Kind == KindSecret && !e.allowedPath(f.path) {
					view.session.rememberSecret(s.Value)
				}
			}
			return nil, nil
		}, nil, nil, e.opt.supportedOnly)
		if err != nil {
			return err
		}
		var source func()
		if e.rules.Sources == "withhold" && !e.opt.supportedOnly {
			source = func() { req.stats.Masked[KindSource]++ }
		}
		masked, err = rewriteRecordedMode(body, m.maskText, source, req.rememberRaw, e.opt.supportedOnly)
		if err != nil {
			return err
		}
		for _, sd := range view.all {
			for _, record := range sd.entries {
				req.undo.add(record.Pseudo, matchValue{real: record.Real, kind: record.Kind, foreign: sd != view.session})
			}
		}
		for _, record := range view.session.entries {
			req.undo.add(record.Pseudo, matchValue{real: record.Real, kind: record.Kind})
		}
		req.issued = &sessionData{key: view.session.key, networks: maps.Clone(view.session.networks)}
		for pseudo, real := range req.secrets {
			req.undo.add(pseudo, matchValue{real: real, kind: KindSecret, secret: true})
		}
		if view.hold != nil {
			req.release = view.hold()
		}
		return nil
	}
	var err error
	if id != "" {
		req.stats.Scope = "session"
		err = e.store.transaction(id, !e.opt.supportedOnly && hasThinking(body), transform)
	} else {
		if !e.opt.supportedOnly && hasThinking(body) {
			return nil, nil, &RejectError{Reason: "словарь сессии отсутствует; начните новую сессию"}
		}
		key, keyErr := e.store.newKey()
		if keyErr != nil {
			return nil, nil, keyErr
		}
		sd := &sessionData{key: key, entries: make(map[string]mapRecord)}
		err = transform(&sessionView{session: sd, all: map[string]*sessionData{"": sd}})
	}
	if err != nil {
		req.Close()
		return nil, nil, err
	}
	e.counts.session(id, req.stats.Version)
	return masked, req, nil
}
func (e *Engine) UnmaskJSON(req *Request, body []byte) ([]byte, error) {
	if req == nil || req.engine != e {
		return nil, errors.New("privacy: foreign or missing request")
	}
	req.mu.Lock()
	defer req.mu.Unlock()
	if req.closed {
		return nil, &RejectError{Reason: "request dictionary is closed"}
	}
	return rewriteRecordedMode(body, req.unmaskText, nil, nil, e.opt.supportedOnly)
}
func (e *Engine) Prune(now time.Time) (int, error) { return e.store.prune(now, e.rules.RetainDays) }
func (e *Engine) Forget(session string) error {
	if session == "" {
		return &RejectError{Reason: "empty session id"}
	}
	return e.store.forget(session)
}
func (e *Engine) ForgetAll() error { return e.store.forget("") }
