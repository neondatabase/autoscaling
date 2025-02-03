package util

import (
	"encoding/base64"
	"encoding/binary"
	"os"
	"path/filepath"
	"sort"

	"golang.org/x/crypto/blake2b"
)

// Calculate the checksum over all files in a directory, assuming the directory is flat (contains no subdirs).
func ChecksumFlatDir(path string) (string, error) {
	files, err := ReadAllFiles(path)
	if err != nil {
		return "", err
	}

	// sort the file names for a reproducible hash
	var keys []string
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	hasher, err := blake2b.New256(nil)
	if err != nil {
		return "", err
	}

	for _, filename := range keys {
		data := files[filename]
		var length []byte

		// hash as "{name}\0{len(data)}{data}"
		// this prevents any possible hash confusion problems
		hasher.Write([]byte(filename))
		hasher.Write([]byte{0})
		hasher.Write(binary.LittleEndian.AppendUint64(length, uint64(len(data))))
		hasher.Write(data)
	}

	sum := hasher.Sum(nil)
	sumBase64 := base64.RawStdEncoding.EncodeToString(sum)
	return sumBase64, nil
}

// Read all files in a directory, assuming the directory is flat (contains no subdirs).
func ReadAllFiles(path string) (map[string][]byte, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}

	output := make(map[string][]byte)

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		data, err := os.ReadFile(filepath.Join(path, entry.Name()))
		if err != nil {
			return nil, err
		}

		output[entry.Name()] = data
	}

	return output, nil
}
