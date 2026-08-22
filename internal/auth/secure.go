// Package auth 认证凭证的读写；此处提供 Windows DPAPI 加密工具，
// 用于对 auth 文件中的 token 字段加密（仅当前 Windows 用户可解密）。
package auth

import (
	"encoding/base64"
	"strings"
	"syscall"
	"unsafe"
)

// dpapiPrefix 加密值前缀：识别加密 token，解密失败/无前缀时按明文处理（兼容旧文件）。
const dpapiPrefix = "dpapi:"

type dataBlob struct {
	cbData uint32
	pbData *byte
}

var (
	kernel32      = syscall.NewLazyDLL("kernel32.dll")
	crypt32       = syscall.NewLazyDLL("crypt32.dll")
	procLocalFree = kernel32.NewProc("LocalFree")
	procProtect   = crypt32.NewProc("CryptProtectData")
	procUnprotect = crypt32.NewProc("CryptUnprotectData")
)

// EncryptSecret 用 DPAPI 加密敏感串，返回 "dpapi:" + base64。
// 加密失败或入参为空时原样返回（降级：不阻塞写文件）。
func EncryptSecret(plain string) string {
	if plain == "" || strings.HasPrefix(plain, dpapiPrefix) {
		return plain
	}
	b := []byte(plain)
	in := dataBlob{cbData: uint32(len(b)), pbData: &b[0]}
	var out dataBlob
	r, _, _ := procProtect.Call(
		uintptr(unsafe.Pointer(&in)), 0, 0, 0, 0, 0, uintptr(unsafe.Pointer(&out)))
	if r == 0 || out.pbData == nil || out.cbData == 0 {
		return plain
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	enc := unsafe.Slice(out.pbData, int(out.cbData))
	return dpapiPrefix + base64.StdEncoding.EncodeToString(enc)
}

// DecryptSecret 解密 "dpapi:" 前缀串；无前缀、格式错误或解密失败均原样返回。
func DecryptSecret(s string) string {
	if !strings.HasPrefix(s, dpapiPrefix) {
		return s
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, dpapiPrefix))
	if err != nil || len(raw) == 0 {
		return s
	}
	in := dataBlob{cbData: uint32(len(raw)), pbData: &raw[0]}
	var out dataBlob
	r, _, _ := procUnprotect.Call(
		uintptr(unsafe.Pointer(&in)), 0, 0, 0, 0, 0, uintptr(unsafe.Pointer(&out)))
	if r == 0 || out.pbData == nil || out.cbData == 0 {
		return s
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	return string(unsafe.Slice(out.pbData, int(out.cbData)))
}
