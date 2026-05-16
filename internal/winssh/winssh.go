// Package winssh is a thin SSH client for talking to Windows runner VMs
// provisioned by krapow.
//
// Host key verification is disabled — VMs are short-lived, ephemeral, and only
// reachable over the Incus host's private bridge (10.36.x or fd42:: ULA).
// We trust the network path more than the host key here.
package winssh

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	user = "Administrator"
	port = "22"
)

// Client wraps an open SSH connection.
type Client struct {
	conn *ssh.Client
}

// Dial connects to addr (host without port) using the private key at privPath.
// Retries connect for `retryFor` to absorb VM boot time.
func Dial(addr, privPath string, retryFor time.Duration) (*Client, error) {
	keyBytes, err := os.ReadFile(privPath)
	if err != nil {
		return nil, fmt.Errorf("read private key %s: %w", privPath, err)
	}
	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}
	deadline := time.Now().Add(retryFor)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := ssh.Dial("tcp", net.JoinHostPort(addr, port), cfg)
		if err == nil {
			return &Client{conn: conn}, nil
		}
		lastErr = err
		time.Sleep(5 * time.Second)
	}
	return nil, fmt.Errorf("ssh connect to %s failed after %s: %w", addr, retryFor, lastErr)
}

func (c *Client) Close() error { return c.conn.Close() }

// StreamOut and StreamErr are where Run echoes subprocess output. Default to
// os.Stdout/Stderr; cmd-level code overrides for TUI mode.
var (
	StreamOut io.Writer = os.Stdout
	StreamErr io.Writer = os.Stderr
)

// Run executes `cmd` on the remote, returning combined stdout+stderr. Streams
// to StreamOut/StreamErr too so long-running commands are observable. CLIXML
// noise (emitted by Windows OpenSSH when the default shell is PowerShell) is
// filtered out of both the streamed and returned text.
func (c *Client) Run(cmd string) (string, error) {
	sess, err := c.conn.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()

	var combined bytes.Buffer
	sess.Stdout = newClixmlFilter(io.MultiWriter(StreamOut, &combined))
	sess.Stderr = newClixmlFilter(io.MultiWriter(StreamErr, &combined))
	err = sess.Run(cmd)
	return combined.String(), err
}

// encodedCmdLimit is a conservative cap on the encoded-command size we'll
// send via `powershell -EncodedCommand`. Windows CreateProcess limits
// lpCommandLine to ~32 KB, and sshd-on-Windows wraps the command in its
// default shell which adds overhead. Headroom of ~8 KB keeps us safe.
const encodedCmdLimit = 24 * 1024

// chunkSize is the raw script chunk size when uploading scripts that exceed
// encodedCmdLimit. After the AppendAllBytes wrapper plus base64 plus UTF-16LE
// re-encoding, 6 KB raw stays comfortably under encodedCmdLimit.
const chunkSize = 6 * 1024

// psFlags are the standard powershell.exe flags we use for every invocation.
//
//	-NonInteractive    don't prompt for any input
//	-OutputFormat Text suppress CLIXML serialization (otherwise every Write-Host
//	                   and progress event gets framed as <Objs ...> XML which
//	                   floods the terminal — see PowerShell-over-SSH protocol).
const psFlags = "-NoProfile -NonInteractive -ExecutionPolicy Bypass -OutputFormat Text"

// RunPowerShell runs a PowerShell script on the remote. Scripts that fit
// inside encodedCmdLimit when UTF-16LE+base64-encoded are sent inline via
// `-EncodedCommand`; larger scripts are uploaded in chunks to a temp file
// and executed with `-File <path>`.
//
// We deliberately do NOT use `-File -` (read script from stdin). On
// sshd-for-Windows, the SSH stdin EOF doesn't propagate through the default
// shell wrapper to the inner powershell process, so `-File -` blocks
// indefinitely after the script is sent.
func (c *Client) RunPowerShell(script string) (string, error) {
	encoded := encodePowerShell(script)
	if len(encoded) <= encodedCmdLimit {
		return c.Run("powershell " + psFlags + " -EncodedCommand " + encoded)
	}
	return c.runPowerShellViaTempFile(script)
}

// runPowerShellViaTempFile uploads the script in chunks to a unique remote
// temp file, executes it with `-File`, and removes the file regardless of
// outcome. Each chunk's upload command stays well under encodedCmdLimit so
// the recursion into RunPowerShell always lands in the inline path.
func (c *Client) runPowerShellViaTempFile(script string) (string, error) {
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("nonce: %w", err)
	}
	remotePath := `C:\Windows\Temp\krapow-` + hex.EncodeToString(nonce[:]) + `.ps1`

	// Prepend a UTF-8 BOM. Windows PowerShell 5.1 reads BOM-less .ps1 files
	// as Windows-1252 / system codepage, which mangles multi-byte UTF-8
	// sequences (em-dashes, smart quotes, etc.) into garbage tokens that
	// produce confusing parser errors far from the actual character.
	raw := append([]byte{0xEF, 0xBB, 0xBF}, []byte(script)...)
	for off := 0; off < len(raw); off += chunkSize {
		end := off + chunkSize
		if end > len(raw) {
			end = len(raw)
		}
		b64 := base64.StdEncoding.EncodeToString(raw[off:end])
		// AppendAllBytes is .NET 6+; Windows PowerShell 5.1 ships .NET Framework
		// 4.x which doesn't have it. Use a FileStream in Append mode instead.
		upload := fmt.Sprintf(
			`$b=[System.Convert]::FromBase64String('%s'); $f=[System.IO.File]::Open('%s','Append','Write'); try { $f.Write($b,0,$b.Length) } finally { $f.Close() }`,
			b64, remotePath,
		)
		if _, err := c.RunPowerShell(upload); err != nil {
			// best-effort cleanup; ignore error since the file may not exist
			_, _ = c.RunPowerShell(fmt.Sprintf(`Remove-Item -Force '%s' -ErrorAction SilentlyContinue`, remotePath))
			return "", fmt.Errorf("upload chunk at offset %d: %w", off, err)
		}
	}

	run := fmt.Sprintf(
		`try { & powershell %s -File '%s'; exit $LASTEXITCODE } finally { Remove-Item -Force '%s' -ErrorAction SilentlyContinue }`,
		psFlags, remotePath, remotePath,
	)
	return c.RunPowerShell(run)
}

// encodePowerShell returns the script encoded as PowerShell's -EncodedCommand
// expects: UTF-16LE bytes, base64-standard, no padding stripped.
func encodePowerShell(script string) string {
	u16 := make([]byte, 0, len(script)*2)
	for _, r := range script {
		if r < 0x10000 {
			u16 = append(u16, byte(r), byte(r>>8))
		} else {
			r -= 0x10000
			hi := 0xD800 + (r >> 10)
			lo := 0xDC00 + (r & 0x3FF)
			u16 = append(u16, byte(hi), byte(hi>>8), byte(lo), byte(lo>>8))
		}
	}
	return base64.StdEncoding.EncodeToString(u16)
}

// clixmlObjs matches a CLIXML envelope on a single line (PowerShell-over-SSH
// emits the whole <Objs ...>...</Objs> blob as one line in practice).
var clixmlObjs = regexp.MustCompile(`<Objs Version="[^"]*" xmlns="[^"]*">.*?</Objs>`)

// clixmlFilter wraps a writer and strips out the CLIXML noise Windows OpenSSH
// adds when its default shell is PowerShell:
//
//	#< CLIXML            ← marker line announcing serialized objects follow
//	<Objs ...>...</Objs> ← the actual serialized progress / info / etc.
//
// We buffer until a newline so we can decide line-by-line.
type clixmlFilter struct {
	dst io.Writer
	buf bytes.Buffer
}

func newClixmlFilter(dst io.Writer) *clixmlFilter { return &clixmlFilter{dst: dst} }

func (f *clixmlFilter) Write(p []byte) (int, error) {
	f.buf.Write(p)
	// Flush complete lines; hold the (possibly partial) trailing line in buf.
	for {
		line, err := f.buf.ReadString('\n')
		if err == io.EOF {
			// No newline yet; put back what we read.
			f.buf.WriteString(line)
			break
		}
		f.emitLine(line)
	}
	return len(p), nil
}

func (f *clixmlFilter) emitLine(line string) {
	// Strip the "#< CLIXML" marker line entirely (it can have trailing CR).
	trimmed := line
	for len(trimmed) > 0 && (trimmed[len(trimmed)-1] == '\n' || trimmed[len(trimmed)-1] == '\r') {
		trimmed = trimmed[:len(trimmed)-1]
	}
	if trimmed == "#< CLIXML" {
		return
	}
	// Strip any CLIXML <Objs ...>...</Objs> blocks that appear inline.
	cleaned := clixmlObjs.ReplaceAllString(line, "")
	// If stripping left only whitespace, drop the line entirely.
	allSpace := true
	for _, r := range cleaned {
		if r != ' ' && r != '\t' && r != '\n' && r != '\r' {
			allSpace = false
			break
		}
	}
	if allSpace {
		return
	}
	f.dst.Write([]byte(cleaned))
}
