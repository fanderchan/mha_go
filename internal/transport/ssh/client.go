package ssh

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"mha-go/internal/domain"

	cryptossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

type SecretResolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

type Executor interface {
	Run(ctx context.Context, node domain.NodeSpec, command string) (stdout, stderr string, err error)
}

type StreamExecutor interface {
	Stream(ctx context.Context, node domain.NodeSpec, command string, stdout io.Writer) (stderr string, err error)
}

type Client struct {
	resolver        SecretResolver
	dialTimeout     time.Duration
	hostKeyCallback cryptossh.HostKeyCallback
}

func NewClient(resolver SecretResolver) *Client {
	return &Client{
		resolver:        resolver,
		dialTimeout:     5 * time.Second,
		hostKeyCallback: defaultHostKeyCallback(),
	}
}

func (c *Client) Run(ctx context.Context, node domain.NodeSpec, command string) (stdout, stderr string, err error) {
	var out bytes.Buffer
	stderr, err = c.Stream(ctx, node, command, &out)
	return out.String(), stderr, err
}

func (c *Client) Stream(ctx context.Context, node domain.NodeSpec, command string, stdout io.Writer) (string, error) {
	if stdout == nil {
		stdout = io.Discard
	}
	client, err := c.dial(ctx, node)
	if err != nil {
		return "", err
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("create SSH session for node %q: %w", node.ID, err)
	}
	defer session.Close()

	stdoutPipe, err := session.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("create SSH stdout pipe for node %q: %w", node.ID, err)
	}
	stderrPipe, err := session.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("create SSH stderr pipe for node %q: %w", node.ID, err)
	}

	if err := session.Start(command); err != nil {
		return "", fmt.Errorf("start SSH command on node %q: %w", node.ID, err)
	}

	var stderr bytes.Buffer
	copyStdout := make(chan error, 1)
	copyStderr := make(chan error, 1)
	waitDone := make(chan error, 1)

	go func() {
		_, err := io.Copy(stdout, stdoutPipe)
		copyStdout <- err
	}()
	go func() {
		_, err := io.Copy(&stderr, stderrPipe)
		copyStderr <- err
	}()
	go func() {
		waitDone <- session.Wait()
	}()

	select {
	case <-ctx.Done():
		_ = client.Close()
		return stderr.String(), ctx.Err()
	case waitErr := <-waitDone:
		stdoutErr := <-copyStdout
		stderrErr := <-copyStderr
		if stdoutErr != nil {
			return stderr.String(), fmt.Errorf("copy SSH stdout from node %q: %w", node.ID, stdoutErr)
		}
		if stderrErr != nil {
			return stderr.String(), fmt.Errorf("copy SSH stderr from node %q: %w", node.ID, stderrErr)
		}
		if waitErr != nil {
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				return stderr.String(), fmt.Errorf("SSH command on node %q failed: %w", node.ID, waitErr)
			}
			return stderr.String(), fmt.Errorf("SSH command on node %q failed: %w: %s", node.ID, waitErr, msg)
		}
		return stderr.String(), nil
	}
}

func (c *Client) dial(ctx context.Context, node domain.NodeSpec) (*cryptossh.Client, error) {
	if node.SSH == nil {
		return nil, fmt.Errorf("node %q has no ssh config", node.ID)
	}
	user := strings.TrimSpace(node.SSH.User)
	if user == "" {
		return nil, fmt.Errorf("node %q ssh.user must be set", node.ID)
	}
	authMethods, err := c.authMethods(ctx, *node.SSH)
	if err != nil {
		return nil, fmt.Errorf("node %q ssh auth: %w", node.ID, err)
	}
	if len(authMethods) == 0 {
		return nil, fmt.Errorf("node %q has no SSH auth method; set ssh.password_ref or ssh.private_key_ref", node.ID)
	}

	port := node.SSH.Port
	if port == 0 {
		port = 22
	}
	addr := net.JoinHostPort(node.Host, strconv.Itoa(port))
	cfg := &cryptossh.ClientConfig{
		User:            user,
		Auth:            authMethods,
		HostKeyCallback: c.hostKeyCallback,
		Timeout:         c.dialTimeout,
	}

	dialer := net.Dialer{Timeout: c.dialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial SSH node %q at %s: %w", node.ID, addr, err)
	}
	sshConn, chans, reqs, err := cryptossh.NewClientConn(conn, addr, cfg)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("handshake SSH node %q at %s: %w", node.ID, addr, err)
	}
	return cryptossh.NewClient(sshConn, chans, reqs), nil
}

func (c *Client) authMethods(ctx context.Context, spec domain.SSHTargetSpec) ([]cryptossh.AuthMethod, error) {
	methods := make([]cryptossh.AuthMethod, 0, 2)
	if strings.TrimSpace(spec.PasswordRef) != "" {
		password, err := c.resolve(ctx, spec.PasswordRef)
		if err != nil {
			return nil, fmt.Errorf("resolve ssh.password_ref: %w", err)
		}
		methods = append(methods, cryptossh.Password(password), cryptossh.KeyboardInteractive(func(_ string, _ string, questions []string, _ []bool) ([]string, error) {
			answers := make([]string, len(questions))
			for i := range answers {
				answers[i] = password
			}
			return answers, nil
		}))
	}
	if strings.TrimSpace(spec.PrivateKeyRef) != "" {
		key, err := c.resolve(ctx, spec.PrivateKeyRef)
		if err != nil {
			return nil, fmt.Errorf("resolve ssh.private_key_ref: %w", err)
		}
		passphrase := ""
		if strings.TrimSpace(spec.PrivateKeyPassphraseRef) != "" {
			passphrase, err = c.resolve(ctx, spec.PrivateKeyPassphraseRef)
			if err != nil {
				return nil, fmt.Errorf("resolve ssh.private_key_passphrase_ref: %w", err)
			}
		}
		signer, err := parsePrivateKey(key, passphrase)
		if err != nil {
			return nil, err
		}
		methods = append(methods, cryptossh.PublicKeys(signer))
	}
	return methods, nil
}

func (c *Client) resolve(ctx context.Context, ref string) (string, error) {
	if c.resolver == nil {
		return "", fmt.Errorf("secret resolver is not configured")
	}
	return c.resolver.Resolve(ctx, ref)
}

func parsePrivateKey(key, passphrase string) (cryptossh.Signer, error) {
	if passphrase != "" {
		signer, err := cryptossh.ParsePrivateKeyWithPassphrase([]byte(key), []byte(passphrase))
		if err == nil {
			return signer, nil
		}
		return nil, fmt.Errorf("parse encrypted ssh private key: %w", err)
	}
	signer, err := cryptossh.ParsePrivateKey([]byte(key))
	if err != nil {
		return nil, fmt.Errorf("parse ssh private key: %w", err)
	}
	return signer, nil
}

func defaultHostKeyCallback() cryptossh.HostKeyCallback {
	if path := strings.TrimSpace(os.Getenv("MHA_SSH_KNOWN_HOSTS")); path != "" {
		if cb, err := knownhosts.New(path); err == nil {
			return cb
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		path := filepath.Join(home, ".ssh", "known_hosts")
		if _, statErr := os.Stat(path); statErr == nil {
			if cb, err := knownhosts.New(path); err == nil {
				return cb
			}
		}
	}
	return cryptossh.InsecureIgnoreHostKey()
}
