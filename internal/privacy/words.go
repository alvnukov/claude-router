package privacy

import (
	"bufio"
	"compress/gzip"
	"embed"
	"sync"
)

//go:embed words/*.txt.gz
var wordFiles embed.FS

// Bloom membership has no false negatives. A false positive merely generates
// another invented name. 16 bits per source word replaces millions of strings
// and hash-map entries with about 6.4 MB shared by every engine.
type wordFilter struct {
	bits []uint64
}

func (f *wordFilter) hash(s string) (uint64, uint64) {
	h := uint64(14695981039346656037)
	for i := range len(s) {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	h ^= h >> 30
	h *= 0xbf58476d1ce4e5b9
	h ^= h >> 27
	h *= 0x94d049bb133111eb
	h ^= h >> 31
	return h, (h<<29 | h>>35) | 1
}
func (f *wordFilter) add(s string) {
	a, b := f.hash(s)
	n := uint64(len(f.bits) * 64)
	for i := uint64(0); i < 10; i++ {
		v := (a + i*b) % n
		f.bits[v/64] |= 1 << (v % 64)
	}
}
func (f *wordFilter) contains(s string) bool {
	a, b := f.hash(s)
	n := uint64(len(f.bits) * 64)
	for i := uint64(0); i < 10; i++ {
		v := (a + i*b) % n
		if f.bits[v/64]&(1<<(v%64)) == 0 {
			return false
		}
	}
	return true
}

var loadWords = sync.OnceValues(func() (*wordFilter, error) {
	f := &wordFilter{bits: make([]uint64, 840000)}
	for _, name := range []string{"surnames_en.txt.gz", "words_en.txt.gz", "words_ru.txt.gz"} {
		file, err := wordFiles.Open("words/" + name)
		if err != nil {
			return nil, err
		}
		gz, err := gzip.NewReader(file)
		if err != nil {
			file.Close()
			return nil, err
		}
		scanner := bufio.NewScanner(gz)
		for scanner.Scan() {
			f.add(scanner.Text())
		}
		err = scanner.Err()
		gz.Close()
		file.Close()
		if err != nil {
			return nil, err
		}
	}
	return f, nil
})
