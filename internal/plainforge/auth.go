package plainforge

import (
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// maxTokenBytes bounds each credentials file read.
const maxTokenBytes = 4096

// SetBasicAuth makes every later request carry HTTP basic auth, which Forgejo
// accepts for Git over HTTP. Call it before the first read or write.
func (h *HTTP) SetBasicAuth(user, token string) {
	h.user, h.token = user, token
}

// LoadCredentials reads the optional credentials from the files "user" and
// "token" in dir, which is where a mounted Secret puts its keys. With dir
// empty, missing, or holding neither file it returns set=false and no error
// (an absent optional Secret). With only one of the two files, or a file that
// cannot be read, is empty or is over 4096 bytes, it returns an error with a
// fixed message that names neither the path nor any content.
func LoadCredentials(dir string) (user, token string, set bool, err error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", "", false, nil
	}
	user, userErr := readSecretFile(filepath.Join(dir, "user"))
	token, tokenErr := readSecretFile(filepath.Join(dir, "token"))
	if errors.Is(userErr, fs.ErrNotExist) && errors.Is(tokenErr, fs.ErrNotExist) {
		return "", "", false, nil
	}
	if errors.Is(userErr, fs.ErrNotExist) || errors.Is(tokenErr, fs.ErrNotExist) {
		return "", "", false, errors.New("the plainforge credentials need both a user and a token")
	}
	if userErr != nil || tokenErr != nil {
		return "", "", false, errors.New("the plainforge credentials could not be read or are empty")
	}
	return user, token, true, nil
}

// readSecretFile returns the trimmed content of a small file. A missing file
// keeps its fs.ErrNotExist; every other failure, and an empty file, is a
// generic error that carries no path or content.
func readSecretFile(path string) (string, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fs.ErrNotExist
	}
	if err != nil {
		return "", errors.New("unreadable")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxTokenBytes+1))
	v := strings.TrimSpace(string(raw))
	if err != nil || len(raw) > maxTokenBytes || v == "" {
		return "", errors.New("unreadable")
	}
	return v, nil
}

func (h *HTTP) authorize(req *http.Request) {
	if h.token != "" {
		req.SetBasicAuth(h.user, h.token)
	}
}
