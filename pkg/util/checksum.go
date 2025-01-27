package util

import (
	"encoding/base64"
	"io"
	"os"

	"golang.org/x/crypto/blake2b"
)

func ChecksumFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil && os.IsNotExist(err) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	defer file.Close()

	hasher, err := blake2b.New256(nil)
	if err != nil {
		return "", err
	}

	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}

	sum := hasher.Sum(nil)
	sumBase64 := base64.RawStdEncoding.EncodeToString(sum)
	return sumBase64, nil
}
