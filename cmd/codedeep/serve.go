package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"codedeep/internal/app"
	"codedeep/internal/config"
	"codedeep/internal/web"
)

func cmdServe(args []string) int {
	fs := flag.NewFlagSet("codedeep serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var cf commonFlags
	cf.register(fs)
	var addr string
	var noOpen bool
	fs.StringVar(&addr, "addr", "", "listen address (default from config)")
	fs.BoolVar(&noOpen, "no-open", false, "do not open a browser")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	cfg, err := config.Load(cf.configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "codedeep:", err)
		return 2
	}
	if err := cf.apply(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "codedeep:", err)
		return 2
	}
	if strings.TrimSpace(addr) != "" {
		cfg.WebAddr = addr
	}
	if !bindIsLoopback(cfg.WebAddr) && cfg.WebToken == "" && !cfg.WebInsecure {
		fmt.Fprintf(os.Stderr, "codedeep: refusing to listen on %s without authentication\n", cfg.WebAddr)
		fmt.Fprintln(os.Stderr, "  set CODEAGENT_WEB_TOKEN (or web_token in the config) to require a token,")
		fmt.Fprintln(os.Stderr, "  or set web_insecure=true if you deliberately want an open server")
		return 1
	}

	if len(cfg.MCPServers) > 0 {
		fmt.Fprintln(os.Stderr, "starting MCP servers…")
	}
	a, err := app.New(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "codedeep:", err)
		return 1
	}
	defer a.Close()

	ln, err := net.Listen("tcp", cfg.WebAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codedeep: cannot listen on %s: %v\n", cfg.WebAddr, err)
		return 1
	}
	url := displayURL(ln.Addr())

	handler := web.New(a).Handler()
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		fmt.Println("\nshutting down…")
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	fmt.Println()
	fmt.Printf("  CodeDeep web UI\n")
	fmt.Printf("  %s\n", url)
	fmt.Printf("  %s\n", styleDim(fmt.Sprintf("%s · %s · approval %s", cfg.Model, cfg.WorkDir, cfg.Approval)))
	if cfg.WebToken != "" {
		fmt.Printf("  %s\n", styleYellow("token required — open the URL and sign in"))
	}
	if !cfg.APIKeyPresent() {
		fmt.Printf("  %s\n", styleYellow("no API key configured — the UI will show setup instructions"))
	}
	fmt.Printf("  %s\n\n", styleDim("Ctrl+C to stop"))

	if host, _, err := net.SplitHostPort(ln.Addr().String()); err == nil {
		if ip := net.ParseIP(host); ip != nil && !ip.IsLoopback() {
			fmt.Fprintf(os.Stderr, "warning: listening on %s without authentication; anyone who can reach this port can run tools\n", host)
		}
	}

	if !noOpen && stdinIsTerminal() {
		go func() {
			time.Sleep(300 * time.Millisecond)
			if err := openBrowser(url); err != nil {
				fmt.Fprintf(os.Stderr, "could not open a browser: %v\n", err)
			}
		}()
	}

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, "codedeep:", err)
		return 1
	}
	fmt.Println("stopped")
	return 0
}

// bindIsLoopback reports whether addr only accepts local connections. An empty
// host means all interfaces, which is not loopback.
func bindIsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.Trim(host, "[]")
	if host == "" || host == "0.0.0.0" || host == "::" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func displayURL(addr net.Addr) string {
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return "http://" + addr.String() + "/"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/"
}

func openBrowser(url string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		return exec.Command("open", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}

func styleDim(s string) string    { return styles{on: colorEnabled(os.Stdout)}.dim(s) }
func styleYellow(s string) string { return styles{on: colorEnabled(os.Stdout)}.yellow(s) }
