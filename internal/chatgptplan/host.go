package chatgptplan

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"localrouter/internal/platform"
	"os"
	"regexp"
	"strings"
)

var hostIDPattern = regexp.MustCompile(`^urn:uuid:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func validHostID(id string) bool { return hostIDPattern.MatchString(id) }
func HostID(ctx context.Context, path string) (string, error) {
	var id string
	err := platform.WithLock(ctx, path+".lock", func() error {
		info, err := os.Lstat(path)
		if err == nil {
			if !info.Mode().IsRegular() || info.Size() > 128 || info.Mode().Perm()&0077 != 0 {
				return errors.New("invalid private host identity file")
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			id = strings.TrimSpace(string(raw))
			if !validHostID(id) {
				return errors.New("invalid host identity")
			}
			return nil
		}
		if !os.IsNotExist(err) {
			return err
		}
		b := make([]byte, 16)
		if _, err = rand.Read(b); err != nil {
			return err
		}
		b[6] = (b[6] & 15) | 64
		b[8] = (b[8] & 63) | 128
		h := hex.EncodeToString(b)
		id = "urn:uuid:" + h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
		return platform.WritePrivateAtomic(path, []byte(id+"\n"))
	})
	return id, err
}
