//go:build linux

package tailscale

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/imksoo/routerd2/internal/engineimage"
	"github.com/imksoo/routerd2/internal/protocol/tailscaleconfig"
	"github.com/imksoo/routerd2/internal/statefs"
	"golang.org/x/sys/unix"
)

// RunEngine replaces the helper with the catalog's daemon. Only its identity
// directory and LocalAPI socket are shared. In particular host resolved/DBus and
// resolv.conf are absent even when upstream clears DNS with CorpDNS=false.
func RunEngine() error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	f := os.NewFile(3, "tailscale-config")
	var cfg tailscaleconfig.Config
	err := json.NewDecoder(io.LimitReader(f, 65536)).Decode(&cfg)
	f.Close()
	if err != nil {
		return err
	}
	if err = tailscaleconfig.Validate(cfg); err != nil {
		return err
	}
	var ca []byte
	if cfg.ControlCAFile != "" {
		ca, err = os.ReadFile(cfg.ControlCAFile)
		if err != nil {
			return err
		}
	}
	if err = unix.Unshare(unix.CLONE_NEWNS | unix.CLONE_FS); err != nil {
		return fmt.Errorf("Tailscale mount namespace: %w", err)
	}
	if err = unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return err
	}
	// Reopen inside the new mount namespace. An inherited directory FD still
	// names a mount in the parent's namespace; Linux rejects binding it here.
	state, err := statefs.OpenDirectory(cfg.StateDir, false)
	if err != nil {
		return err
	}
	defer state.Close()
	run, err := statefs.OpenDirectory(cfg.RuntimeDir, false)
	if err != nil {
		return err
	}
	defer run.Close()
	if err = unix.Mount("tmpfs", "/tmp", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=700"); err != nil {
		return err
	}
	root, err := os.MkdirTemp("/tmp", "tailscale-")
	if err != nil {
		return err
	}
	// Bind the private directories before chroot. Neither an
	// operator command nor host /run is accessible in the engine filesystem.
	for _, bind := range []struct{ source, target string }{
		{"/proc/thread-self/fd/" + strconv.Itoa(int(state.Fd())), "/state"}, {"/proc/thread-self/fd/" + strconv.Itoa(int(run.Fd())), "/run/tailscale"}, {"/proc", "/proc"}, {"/sys", "/sys"},
	} {
		target := root + bind.target
		if err = os.MkdirAll(target, 0700); err != nil {
			return err
		}
		if err = unix.Mount(bind.source, target, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			return fmt.Errorf("Tailscale bind %s: %w", bind.target, err)
		}
	}
	if err = os.MkdirAll(root+"/dev/net", 0700); err != nil {
		return err
	}
	for _, dev := range []string{"net/tun", "null", "urandom"} {
		target := root + "/dev/" + dev
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		f.Close()
		if err = unix.Mount("/dev/"+dev, target, "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("Tailscale device %s: %w", dev, err)
		}
	}
	if err = unix.Chroot(root); err != nil {
		return err
	}
	if err = os.Chdir("/"); err != nil {
		return err
	}
	image := os.NewFile(4, "tailscale-image")
	err = engineimage.Extract(image)
	image.Close()
	if err != nil {
		return err
	}
	if err = os.MkdirAll("/tmp", 0700); err != nil {
		return err
	}
	if err = os.MkdirAll("/etc", 0700); err != nil {
		return err
	}
	var resolver strings.Builder
	for _, server := range cfg.BootstrapDNS {
		fmt.Fprintln(&resolver, "nameserver", server)
	}
	if err = os.WriteFile("/etc/resolv.conf", []byte(resolver.String()), 0600); err != nil {
		return err
	}
	if len(ca) != 0 {
		if err = os.MkdirAll("/etc/ssl/certs", 0700); err != nil {
			return err
		}
		if err = os.WriteFile("/etc/ssl/certs/control.pem", ca, 0600); err != nil {
			return err
		}
	}
	if err = os.WriteFile("/bootstrap.json", []byte(`{"version":"alpha0","locked":false,"enabled":false}`), 0600); err != nil {
		return err
	}
	env := []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "TS_DEBUG_FIREWALL_MODE=nftables", "TS_LOGS_DIR=/run/tailscale"}
	if len(ca) != 0 {
		env = append(env, "SSL_CERT_FILE=/etc/ssl/certs/control.pem")
	}
	err = syscall.Exec("/usr/sbin/tailscaled", []string{"tailscaled", "--state=/state/tailscaled.state", "--statedir=/state", "--socket=/run/tailscale/local.sock", "--tun=" + cfg.Interface, "--port=41641", "--no-logs-no-support", "--config=/bootstrap.json"}, env)
	return fmt.Errorf("execute bundled tailscaled: %w", err)
}

type process struct {
	cmd       *exec.Cmd
	done      chan struct{}
	err       error
	client    *http.Client
	transport *http.Transport
}

// The worker pins its OS thread through close: PDEATHSIG follows that thread.
func start(ctx context.Context, cfg tailscaleconfig.Config, image *os.File) (*process, error) {
	fd, err := unix.MemfdCreate("tailscale-config", unix.MFD_CLOEXEC)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "tailscale-config")
	defer f.Close()
	if err = json.NewEncoder(f).Encode(cfg); err != nil {
		return nil, err
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	// The explicit owner closes the process after its loop; cancellation alone
	// must not detach it or leave a successful stale observation behind.
	cmd := exec.Command("/proc/self/exe", "tailscale-engine")
	cmd.ExtraFiles = []*os.File{f, image}
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	sock := filepath.Join(cfg.RuntimeDir, "local.sock")
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}
	p := &process{cmd: cmd, done: make(chan struct{}), transport: transport, client: &http.Client{Transport: transport, Timeout: 2 * time.Second}}
	go func() { p.err = cmd.Wait(); close(p.done) }()
	startup, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		conn, err := (&net.Dialer{}).DialContext(startup, "unix", sock)
		if err == nil {
			conn.Close()
			return p, nil
		}
		select {
		case <-p.done:
			p.close()
			return nil, errors.Join(errors.New("tailscaled exited before its socket"), p.err)
		case <-startup.Done():
			p.close()
			return nil, errors.New("tailscaled did not open its socket: " + startup.Err().Error())
		case <-ticker.C:
		}
	}
}
func (p *process) close() {
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
	p.transport.CloseIdleConnections()
}

func (p *process) request(ctx context.Context, method, path string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://local-tailscaled.sock/localapi/v0/"+path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// Do not echo upstream error bodies: authentication material is private.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errors.New("Tailscale LocalAPI " + path + " HTTP " + strconv.Itoa(resp.StatusCode))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err == nil && len(data) > 1<<20 {
		err = errors.New("Tailscale status exceeds publication bound")
	}
	return data, err
}
