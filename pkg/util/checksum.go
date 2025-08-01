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

	// note: any changes to the hash need to be sychronised between neonvm-runner and neonvm-daemon.
	// Since they are updated independently, this is not trivial.
	// If in doubt, make a new function and don't touch this one.
	hasher, err := blake2b.New256(nil)
	if err != nil {
		return "", err
	}

	for _, filename := range keys {
		data := files[filename]

		// File hash with the following encoding: "{name}\0{len(data)}{data}".
		//
		// This format prevents any possible (even if unrealistic) hash confusion problems.
		// If we only hashed filename and data, then there's no difference between:
		// 	 name = "file1"
		// 	 data = []
		// and
		//   name = "file"
		//   data = [b'1']
		//
		// We are trusting that filenames on linux cannot have a nul character.
		hasher.Write([]byte(filename))
		hasher.Write([]byte{0})
		hasher.Write(binary.LittleEndian.AppendUint64([]byte{}, uint64(len(data))))
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
