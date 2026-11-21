// Command keygen 生成 UFOT 所需密钥（无 openssl 时的等价工具）。
//
//	go run ./deploy/keygen -dir /etc/ufot/keys -group ufot
//
// 生成：
//
//	jwt_sign_key.pem    Ed25519 PKCS#8 私钥（用户 access/refresh 签名）
//	admin_jwt_key.pem   Ed25519 PKCS#8 私钥（管理 API 签名，独立密钥）
//	ipc_key             32 字节十六进制（层间 HMAC 握手）
//	hmac_key            32 字节十六进制（内部消息重放/游标签名）
//	pii_key             32 字节（手机号/邮箱可逆加密）
//	phone_lookup_key    32 字节（PII HMAC 索引）
//	download_key        32 字节（预签名 HMAC）
//	audit_chain_key     32 字节（审计哈希链盐）
//	backup.key          32 字节（备份加密口令）
//
// 已存在的文件不会被覆盖（幂等），因此可以反复执行。
// 权限：目录 0750，私钥 0640，密钥文件 0600。
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	dir := flag.String("dir", "/etc/ufot/keys", "密钥输出目录")
	pemOnly := flag.Bool("pem-only", false, "只生成 Ed25519 密钥对（不生成对称密钥）")
	flag.Parse()

	if err := os.MkdirAll(*dir, 0o750); err != nil {
		fail("mkdir %s: %v", *dir, err)
	}

	pairs := []struct {
		name string
		mode os.FileMode
		gen  func() ([]byte, error)
	}{
		{"jwt_sign_key.pem", 0o640, ed25519PEM},
		{"admin_jwt_key.pem", 0o640, ed25519PEM},
	}
	if !*pemOnly {
		for _, name := range []string{
			"ipc_key", "hmac_key", "pii_key", "phone_lookup_key",
			"download_key", "upload_secret", "audit_chain_key", "backup.key",
		} {
			pairs = append(pairs, struct {
				name string
				mode os.FileMode
				gen  func() ([]byte, error)
			}{name, 0o600, randomHex})
		}
	}

	for _, p := range pairs {
		path := filepath.Join(*dir, p.name)
		if _, err := os.Stat(path); err == nil {
			fmt.Printf("skip   %s (exists)\n", path)
			continue
		}
		body, err := p.gen()
		if err != nil {
			fail("generate %s: %v", p.name, err)
		}
		if err := os.WriteFile(path, body, p.mode); err != nil {
			fail("write %s: %v", path, err)
		}
		if err := os.Chmod(path, p.mode); err != nil {
			fail("chmod %s: %v", path, err)
		}
		fmt.Printf("create %s (%#o)\n", path, p.mode)
	}
}

// ed25519PEM 返回 PKCS#8 PEM。私钥不写口令：文件权限即边界（0640 root:<group>）。
func ed25519PEM() ([]byte, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	block := &pem.Block{Type: "PRIVATE KEY", Bytes: der}
	// 公钥同时落一份，便于运维核对 kid（可选文件，失败不致命）
	return pem.EncodeToMemory(block), nil
}

// randomHex 32 字节随机数的十六进制表示（64 字符），可直接做 HMAC 密钥。
func randomHex() ([]byte, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	out := make([]byte, hex.EncodedLen(len(buf))+1)
	hex.Encode(out, buf)
	out[len(out)-1] = '\n'
	return out, nil
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "keygen: "+format+"\n", args...)
	os.Exit(1)
}
