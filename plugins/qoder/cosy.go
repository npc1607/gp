package qoder

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const customAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
const stdAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
const padChar = '$'

var stdToCustom [256]byte
var customToStd [256]byte

const serverPubPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`

var serverPub *rsa.PublicKey

func init() {
	for i := range 64 {
		stdToCustom[stdAlphabet[i]] = customAlphabet[i]
		customToStd[customAlphabet[i]] = stdAlphabet[i]
	}
	stdToCustom['='] = padChar
	customToStd[padChar] = '='

	block, _ := pem.Decode([]byte(serverPubPEM))
	if block == nil {
		panic("qoder: cannot decode server public key")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		panic("qoder: cannot parse server public key: " + err.Error())
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		panic("qoder: server key is not RSA")
	}
	serverPub = rsaPub
}

// EncodeBody 对请求体应用 Qoder 自定义三段旋转 Base64 编码
func EncodeBody(plain []byte) string {
	std := base64.StdEncoding.EncodeToString(plain)
	n := len(std)
	a := n / 3
	var b strings.Builder
	b.Grow(n)
	b.WriteString(std[n-a:])
	b.WriteString(std[a : n-a])
	b.WriteString(std[:a])
	out := []byte(b.String())
	for i, c := range out {
		out[i] = stdToCustom[c]
	}
	return string(out)
}

// DecodeBody 逆向还原 EncodeBody（用于测试验证）
func DecodeBody(encoded string) ([]byte, error) {
	out := []byte(encoded)
	for i, c := range out {
		out[i] = customToStd[c]
	}
	std := string(out)
	n := len(std)
	a := n / 3
	original := std[n-a:] + std[a:n-a] + std[:a]
	return base64.StdEncoding.DecodeString(original)
}

func encryptAESKey(tempKey []byte) (string, error) {
	//nolint:staticcheck // upstream Qoder handshake requires RSA PKCS1v15
	c, err := rsa.EncryptPKCS1v15(rand.Reader, serverPub, tempKey)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(c), nil
}

func encryptInfo(plain, tempKey []byte) (string, error) {
	block, err := aes.NewCipher(tempKey)
	if err != nil {
		return "", err
	}
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	buf := make([]byte, len(plain)+pad)
	copy(buf, plain)
	for i := len(plain); i < len(buf); i++ {
		buf[i] = byte(pad)
	}
	cipher.NewCBCEncrypter(block, tempKey).CryptBlocks(buf, buf)
	return base64.StdEncoding.EncodeToString(buf), nil
}

func newTempKey() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func randomUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func sortedCompactJSON(m map[string]string) []byte {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		vb, _ := json.Marshal(m[k])
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vb)
	}
	b.WriteByte('}')
	return []byte(b.String())
}

func stableHexHash(seed string, bytesLen int) string {
	h := sha256.Sum256([]byte(seed))
	s := hex.EncodeToString(h[:])
	if bytesLen*2 <= len(s) {
		return s[:bytesLen*2]
	}
	return s
}

// cosySession 保存单个 Qoder 账号的 COSY 签名会话状态
type cosySession struct {
	UID          string
	UserType     string
	MachineID    string
	MachineToken string
	MachineType  string
	Hostname     string
	AccessToken  string
	TempKey      string
	CosyKey      string
	Info         string
}

func newCosySession(uid, nickname, accessToken, refreshToken, userType, orgID, orgName string) (*cosySession, error) {
	if userType == "" {
		userType = "personal_professional_trial"
	}
	tempKey := newTempKey()
	cosyKey, err := encryptAESKey([]byte(tempKey))
	if err != nil {
		return nil, err
	}
	identity := map[string]string{
		"name":                 nickname,
		"aid":                  uid,
		"uid":                  uid,
		"yx_uid":               "",
		"organization_id":      orgID,
		"organization_name":    orgName,
		"user_type":            userType,
		"security_oauth_token": accessToken,
		"refresh_token":        refreshToken,
	}
	info, err := encryptInfo(sortedCompactJSON(identity), []byte(tempKey))
	if err != nil {
		return nil, err
	}
	return &cosySession{
		UID:          uid,
		UserType:     userType,
		MachineID:    stableHexHash("mid:"+uid, 32),
		MachineToken: stableHexHash("mtok:"+uid, 32),
		MachineType:  "MacBookPro",
		Hostname:     "gp-host-" + stableHexHash("host:"+uid, 4),
		AccessToken:  accessToken,
		TempKey:      tempKey,
		CosyKey:      cosyKey,
		Info:         info,
	}, nil
}

func (s *cosySession) Headers(body, rawURL, modelKey string) map[string]string {
	path := rawURL
	if u, err := url.Parse(rawURL); err == nil && u.Path != "" {
		path = u.Path
	}
	path = strings.TrimPrefix(path, "/algo")
	payload := map[string]string{
		"cosyVersion": "1.1.75",
		"ideVersion":  "",
		"info":        s.Info,
		"requestId":   randomUUID(),
		"version":     "v1",
	}
	payloadB64 := base64.StdEncoding.EncodeToString(sortedCompactJSON(payload))
	date := strconv.FormatInt(time.Now().Unix(), 10)
	raw := strings.Join([]string{payloadB64, s.CosyKey, date, body, path}, "\n")
	sum := md5.Sum([]byte(raw))
	bearer := "Bearer COSY." + payloadB64 + "." + hex.EncodeToString(sum[:])

	h := map[string]string{
		"cosy-data-policy":      "agree",
		"content-type":          "application/json",
		"cosy-business-product": "cli",
		"cosy-business-type":    "agent",
		"cosy-scene":            "assistant",
		"cosy-clienttype":       "5",
		"cosy-date":             date,
		"cosy-user":             s.UID,
		"cosy-key":              s.CosyKey,
		"accept":                "text/event-stream",
		"authorization":         bearer,
		"accept-encoding":       "identity",
		"cosy-version":          "1.1.75",
		"cosy-machineid":        s.MachineID,
		"cosy-machinetoken":     s.MachineToken,
		"cosy-machinetype":      s.MachineType,
		"cosy-clientip":         "169.254.198.161",
		"login-version":         "v2",
		"user-agent":            "undici",
		"cosy-machineos":        "x86_64_win32",
		"cosy-machinehostname":  s.Hostname,
		"cache-control":         "no-cache",
	}
	if modelKey != "" {
		h["x-model-key"] = modelKey
		h["x-model-source"] = "system"
	}
	return h
}
