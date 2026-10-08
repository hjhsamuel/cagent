// Command modelkey 从标准输入读取 API key 或旧密文，输出当前版本 AES-GCM 密文。
package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/hjhsamuel/cagent/internal/config"
	"github.com/hjhsamuel/cagent/internal/storage/schema"
)

func main() {
	weight := flag.Int64("weight", 1, "API key selection weight; zero disables selection")
	rotate := flag.Bool("rotate", false, "read an EncryptedKey JSON object and re-encrypt with the active version, preserving weight")
	legacy := flag.Bool("legacy", false, "with -rotate, migrate the old combined nonce/ciphertext format")
	flag.Parse()
	if err := run(os.Stdin, os.Stdout, *weight, *rotate, *legacy, os.LookupEnv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(input io.Reader, output io.Writer, weight int64, rotate, legacy bool, lookup func(string) (string, bool)) error {
	if legacy && !rotate {
		return fmt.Errorf("-legacy requires -rotate")
	}
	keys, _ := lookup("CAGENT_MODEL_ENCRYPTION_KEYS")
	version, _ := lookup("CAGENT_MODEL_ENCRYPTION_ACTIVE_VERSION")
	ring, err := config.NewKeyring(config.ModelEncryption{KeysJSON: keys, ActiveVersion: version})
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(input, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return fmt.Errorf("cannot read API key input within size limit")
	}
	plaintext := strings.TrimRight(string(data), "\r\n")
	keyID := ""
	if rotate {
		var old schema.EncryptedKey
		if json.Unmarshal(data, &old) != nil {
			return fmt.Errorf("invalid encrypted API key JSON")
		}
		if legacy {
			// 固定 AES-GCM 的旧格式为 12-byte nonce || ciphertext；运行时不兼容回退。
			combined, err := base64.StdEncoding.DecodeString(old.Ciphertext)
			if err != nil || len(combined) < 12+16 || old.Nonce != "" {
				return fmt.Errorf("invalid legacy encrypted API key")
			}
			old.Nonce = base64.StdEncoding.EncodeToString(combined[:12])
			old.Ciphertext = base64.StdEncoding.EncodeToString(combined[12:])
		}
		plaintext, err = ring.Decrypt(old)
		if err != nil {
			return err
		}
		weight = old.Weight
		keyID = config.StoredKeyID(old)
	}
	key, err := ring.Encrypt(plaintext, weight)
	if err != nil {
		return err
	}
	if keyID != "" {
		key.ID = keyID
	}
	return json.NewEncoder(output).Encode(key)
}
