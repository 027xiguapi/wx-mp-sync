package proxy

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// CertName 是根证书的 CN,用于证书的安装、检查与卸载识别。
const CertName = "wx-mp-sync"

// RootCA 保存根 CA 的 PEM 证书与私钥。
type RootCA struct {
	Cert       []byte // PEM
	PrivateKey []byte // PEM
	Dir        string // 持久化目录
}

// CertPath 返回 CA 证书文件路径。
func (c *RootCA) CertPath() string { return filepath.Join(c.Dir, "ca.crt") }

// KeyPath 返回 CA 私钥文件路径。
func (c *RootCA) KeyPath() string { return filepath.Join(c.Dir, "ca.key") }

// CertDER 返回证书的 DER 原始字节(供 certutil 安装)。
func (c *RootCA) CertDER() ([]byte, error) {
	block, _ := pem.Decode(c.Cert)
	if block == nil {
		return nil, errors.New("无法解析 CA 证书 PEM")
	}
	return block.Bytes, nil
}

// LoadOrCreateRootCA 从 dir 加载 CA;不存在则生成并持久化到 ca.crt / ca.key。
func LoadOrCreateRootCA(dir string) (*RootCA, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")
	certData, certErr := os.ReadFile(certPath)
	keyData, keyErr := os.ReadFile(keyPath)
	if certErr == nil && keyErr == nil && len(certData) > 0 && len(keyData) > 0 {
		return &RootCA{Cert: certData, PrivateKey: keyData, Dir: dir}, nil
	}

	certPEM, keyPEM, err := GenerateRootCA(CertName, 0)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(certPath, certPEM, 0644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		return nil, err
	}
	return &RootCA{Cert: certPEM, PrivateKey: keyPEM, Dir: dir}, nil
}

// GenerateRootCA 生成一个自签根证书,返回 PEM 编码的证书与私钥。
// 移植自参考项目 pkg/certificate/generate.go。
func GenerateRootCA(name string, validFor time.Duration) ([]byte, []byte, error) {
	if name == "" {
		name = CertName
	}
	if validFor <= 0 {
		validFor = 10 * 365 * 24 * time.Hour
	}

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:   name,
			Organization: []string{name},
		},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(validFor),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return nil, nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)})
	return certPEM, keyPEM, nil
}
