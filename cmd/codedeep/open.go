package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"codedeep/internal/config"
	"codedeep/internal/editor"
)

func cmdOpen(args []string) int {
	fs := flag.NewFlagSet("codedeep open", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var configPath, cwd string
	var line int
	fs.StringVar(&configPath, "config", "", "config file path")
	fs.StringVar(&cwd, "cwd", "", "workspace directory")
	fs.IntVar(&line, "line", 0, "line number to open")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "codedeep:", err)
		return 1
	}
	if strings.TrimSpace(cwd) != "" {
		abs, err := filepath.Abs(cwd)
		if err != nil {
			fmt.Fprintln(os.Stderr, "codedeep:", err)
			return 1
		}
		cfg.WorkDir = abs
	}
	ref := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if line > 0 && !strings.HasSuffix(ref, fmt.Sprintf(":%d", line)) {
		ref += ":" + fmt.Sprint(line)
	}
	target, err := editor.Resolve(cfg.WorkDir, ref)
	if err != nil {
		fmt.Fprintln(os.Stderr, "codedeep:", err)
		return 1
	}
	if err := editor.Open(cfg, target); err != nil {
		fmt.Fprintln(os.Stderr, "codedeep:", err)
		return 1
	}
	where := target.Path
	if target.Line > 0 {
		where += fmt.Sprintf(":%d", target.Line)
	}
	fmt.Println("opened", where)
	return 0
}
