package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type tailscaleTLS struct {
	name, cli, dir string
	mu             sync.RWMutex
	cert           *tls.Certificate
}

func defaultTailscaleCLI() string {
	appCLI := "/Applications/Tailscale.app/Contents/MacOS/Tailscale"
	if _, err := os.Stat(appCLI); err == nil {
		return appCLI
	}
	return "tailscale"
}

func newTailscaleTLS(name, cli, dir string) (*tailscaleTLS, error) {
	if name == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, cli, "status", "--json").Output()
		if err != nil {
			return nil, fmt.Errorf("tailscale status: %w", err)
		}
		var status struct{ Self struct{ DNSName string } }
		if err := json.Unmarshal(out, &status); err != nil {
			return nil, err
		}
		name = strings.TrimSuffix(status.Self.DNSName, ".")
	}
	if name == "" {
		return nil, fmt.Errorf("Tailscale DNS name unavailable; enable MagicDNS")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	m := &tailscaleTLS{name: name, cli: cli, dir: dir}
	if err := m.refresh(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *tailscaleTLS) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cert, nil
}

func (m *tailscaleTLS) renewLoop() {
	tick := time.NewTicker(6 * time.Hour)
	defer tick.Stop()
	for range tick.C {
		if err := m.refresh(); err != nil {
			log.Printf("Tailscale certificate renewal: %v", err)
		}
	}
}

func (m *tailscaleTLS) refresh() error {
	certPath, keyPath := filepath.Join(m.dir, "tailscale.crt"), filepath.Join(m.dir, "tailscale.key")
	var existing *tls.Certificate
	if c, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		if leaf, err := x509.ParseCertificate(c.Certificate[0]); err == nil && leaf.VerifyHostname(m.name) == nil && time.Now().Before(leaf.NotAfter) {
			existing = &c
			if time.Until(leaf.NotAfter) > 30*24*time.Hour {
				m.set(&c)
				return nil
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, m.cli, "cert", "--cert-file", "-", "--key-file", "-", m.name).Output()
	if err != nil {
		if existing != nil {
			m.set(existing)
			log.Printf("Tailscale certificate renewal failed; retaining valid certificate: %v", err)
			return nil
		}
		return fmt.Errorf("Tailscale certificate for %s: %w (enable HTTPS Certificates in the Tailscale DNS admin page)", m.name, err)
	}
	var certPEM, keyPEM []byte
	for len(out) > 0 {
		block, rest := pem.Decode(out)
		if block == nil {
			return fmt.Errorf("invalid Tailscale PEM output")
		}
		encoded := pem.EncodeToMemory(block)
		if block.Type == "CERTIFICATE" {
			certPEM = append(certPEM, encoded...)
		}
		if strings.Contains(block.Type, "PRIVATE KEY") {
			keyPEM = append(keyPEM, encoded...)
		}
		out = rest
	}
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("Tailscale certificate/key: %w", err)
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return err
	}
	if err := leaf.VerifyHostname(m.name); err != nil {
		return err
	}
	if !time.Now().Before(leaf.NotAfter) {
		return fmt.Errorf("Tailscale certificate is expired")
	}
	if err := os.WriteFile(certPath, certPEM, 0644); err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		return err
	}
	m.set(&c)
	return nil
}

func (m *tailscaleTLS) set(c *tls.Certificate) { m.mu.Lock(); m.cert = c; m.mu.Unlock() }
