package privacy

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

func subkey(key []byte, domain string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(domain))
	return h.Sum(nil)
}

type pseudonyms struct {
	words     *wordFilter
	reserved  map[string]bool
	endings   []string
	candidate func(string, []byte, int) string
}

func (p *pseudonyms) collision(s string) bool {
	s = strings.ToLower(s)
	if p.reserved[s] || p.words.contains(s) {
		return true
	}
	// Check declensional neighbours against both the corpus and issued names.
	for _, e := range append([]string{""}, p.endings...) {
		if !strings.HasSuffix(s, e) {
			continue
		}
		base := strings.TrimSuffix(s, e)
		if len(base) < 2 {
			continue
		}
		if p.reserved[base] || p.words.contains(base) {
			return true
		}
		for _, suffix := range p.endings {
			v := base + suffix
			if p.reserved[v] || p.words.contains(v) {
				return true
			}
		}
	}
	return false
}
func (p *pseudonyms) issue(real string, key []byte) (string, error) {
	for attempt := 0; attempt < 4096; attempt++ {
		candidate := invent(real, key, attempt)
		if p.candidate != nil {
			candidate = p.candidate(real, key, attempt)
		}
		if candidate != "" && !p.collision(candidate) {
			p.reserved[strings.ToLower(candidate)] = true
			return candidate, nil
		}
	}
	return "", errors.New("privacy: pseudonym space exhausted")
}
func invent(real string, key []byte, attempt int) string {
	h := hmac.New(sha256.New, key)
	fmt.Fprintf(h, "%s\x00%d", strings.ToLower(real), attempt)
	seed := h.Sum(nil)
	var out strings.Builder
	run := []rune{}
	flush := func() {
		if len(run) == 0 {
			return
		}
		cyr := unicode.In(run[0], unicode.Cyrillic)
		cons, vowels := "bcdfghjklmnprstvz", "aeiou"
		if cyr {
			cons, vowels = "бвгджзклмнпрстфхцчш", "аеиоуэюя"
		}
		cs, vs := []rune(cons), []rune(vowels)
		n := max(10, len(run))
		for i := 0; i < n; i++ {
			x := seed[i%len(seed)]
			if i%2 == 0 {
				out.WriteRune(cs[int(x)%len(cs)])
			} else {
				out.WriteRune(vs[int(x)%len(vs)])
			}
		}
		seed = subkey(seed, "next")
		run = nil
	}
	for _, r := range real {
		if unicode.IsLetter(r) {
			run = append(run, r)
		} else {
			flush()
			if unicode.IsDigit(r) {
				out.WriteByte('0' + seed[0]%10)
				seed = subkey(seed, "digit")
			} else {
				out.WriteRune(r)
			}
		}
	}
	flush()
	return caseLike(out.String(), real)
}
func caseLike(value, original string) string {
	if strings.ToUpper(original) == original {
		return strings.ToUpper(value)
	}
	if strings.ToLower(original) == original {
		return strings.ToLower(value)
	}
	words := []rune(value)
	pattern := []rune(original)
	for i := range words {
		if i < len(pattern) && unicode.IsUpper(pattern[i]) {
			words[i] = unicode.ToUpper(words[i])
		} else {
			words[i] = unicode.ToLower(words[i])
		}
	}
	return string(words)
}
func phonePseudo(real string, key []byte, attempt int) string {
	h := hmac.New(sha256.New, key)
	fmt.Fprintf(h, "%s\x00%d", real, attempt)
	sum := h.Sum(nil)
	counter := uint64(0)
	out := []byte(real)
	for i := range out {
		if out[i] >= '0' && out[i] <= '9' {
			out[i] = '0' + sum[counter%32]%10
			counter++
			if counter%32 == 0 {
				var n [8]byte
				binary.BigEndian.PutUint64(n[:], counter)
				h.Write(n[:])
				sum = h.Sum(nil)
			}
		}
	}
	return string(out)
}
