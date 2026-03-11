package commands

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"time"

	refreshengine "github.com/atterpac/refresh/engine"
	refreshprocess "github.com/atterpac/refresh/process"
	"github.com/wailsapp/wails/v3/internal/flags"
	"gopkg.in/yaml.v3"
)

const defaultVitePort = 9245
const wailsVitePort = "WAILS_VITE_PORT"

type devCommandExit struct {
	execute refreshprocess.Execute
	err     error
}

type DevOptions struct {
	flags.Common

	Config   string `description:"The config file including path" default:"./build/config.yml"`
	VitePort int    `name:"port" description:"Specify the vite dev server port"`
	Secure   bool   `name:"s" description:"Enable HTTPS"`
}

func Dev(options *DevOptions) error {
	host := "localhost"

	// flag takes precedence over environment variable
	var port int
	if options.VitePort != 0 {
		port = options.VitePort
	} else if p, err := strconv.Atoi(os.Getenv(wailsVitePort)); err == nil {
		port = p
	} else {
		port = defaultVitePort
	}

	// check if port is already in use
	l, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, port))
	if err != nil {
		return err
	}
	if err = l.Close(); err != nil {
		return err
	}

	// Set environment variable for the dev:frontend task
	os.Setenv(wailsVitePort, strconv.Itoa(port))

	// Set url of frontend dev server
	if options.Secure {
		os.Setenv("FRONTEND_DEVSERVER_URL", fmt.Sprintf("https://%s:%d", host, port))
	} else {
		os.Setenv("FRONTEND_DEVSERVER_URL", fmt.Sprintf("http://%s:%d", host, port))
	}

	return runDevModeOnce(options.Config)
}

func runDevModeOnce(configPath string) error {
	type devConfig struct {
		Config refreshengine.Config `yaml:"dev_mode"`
	}

	var devconfig devConfig

	contents, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(contents, &devconfig); err != nil {
		return err
	}

	return executeDevModeOnce(devconfig.Config)
}

func executeDevModeOnce(config refreshengine.Config) error {
	if len(config.ExecStruct) == 0 {
		return errors.New("dev mode requires at least one execute entry")
	}

	rootDir, err := filepath.Abs(config.RootPath)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	exitCh := make(chan devCommandExit, len(config.ExecStruct))
	running := make([]*exec.Cmd, 0, len(config.ExecStruct))
	defer func() {
		for _, cmd := range running {
			_ = killDevCommand(cmd)
		}
	}()

	primarySeen := false
	for _, execute := range config.ExecStruct {
		switch execute.Type {
		case refreshprocess.Blocking, refreshprocess.Once:
			if err := runDevCommand(ctx, rootDir, execute); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("execute %q failed: %w", execute.Cmd, err)
			}
			if execute.DelayNext > 0 {
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(time.Duration(execute.DelayNext) * time.Millisecond):
				}
			}
		case refreshprocess.Background:
			cmd, err := startDevCommand(rootDir, execute)
			if err != nil {
				return fmt.Errorf("background execute %q failed to start: %w", execute.Cmd, err)
			}
			running = append(running, cmd)
			monitorDevCommand(ctx, cmd, execute, exitCh)
			if execute.DelayNext > 0 {
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(time.Duration(execute.DelayNext) * time.Millisecond):
				}
			}
		case refreshprocess.Primary:
			if primarySeen {
				return errors.New("dev mode only supports one primary execute")
			}
			primarySeen = true
			cmd, err := startDevCommand(rootDir, execute)
			if err != nil {
				return fmt.Errorf("primary execute %q failed to start: %w", execute.Cmd, err)
			}
			running = append(running, cmd)
			monitorDevCommand(ctx, cmd, execute, exitCh)
		default:
			return fmt.Errorf("unsupported dev execute type %q", execute.Type)
		}
	}

	if !primarySeen {
		return errors.New("dev mode requires one primary execute")
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case exit := <-exitCh:
			if exit.execute.Type == refreshprocess.Primary {
				if exit.err != nil {
					return fmt.Errorf("primary execute %q failed: %w", exit.execute.Cmd, exit.err)
				}
				stop()
				return nil
			}
			if ctx.Err() == nil {
				if exit.err != nil {
					return fmt.Errorf("background execute %q failed: %w", exit.execute.Cmd, exit.err)
				}
				return fmt.Errorf("background execute %q exited unexpectedly", exit.execute.Cmd)
			}
		}
	}
}

func runDevCommand(ctx context.Context, rootDir string, execute refreshprocess.Execute) error {
	cmd, err := startDevCommand(rootDir, execute)
	if err != nil {
		return err
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = killDevCommand(cmd)
		<-done
		return nil
	}
}

func startDevCommand(rootDir string, execute refreshprocess.Execute) (*exec.Cmd, error) {
	cmd := newDevCommand(execute.Cmd)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Dir = rootDir
	if execute.ChangeDir != "" {
		cmd.Dir = filepath.Join(rootDir, execute.ChangeDir)
	}
	configureDevCommand(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

func monitorDevCommand(ctx context.Context, cmd *exec.Cmd, execute refreshprocess.Execute, exitCh chan<- devCommandExit) {
	go func() {
		done := make(chan error, 1)
		go func() {
			done <- cmd.Wait()
		}()

		select {
		case err := <-done:
			exitCh <- devCommandExit{execute: execute, err: err}
		case <-ctx.Done():
			_ = killDevCommand(cmd)
			<-done
		}
	}()
}

func newDevCommand(command string) *exec.Cmd {
	if os.PathSeparator == '\\' {
		return exec.Command("cmd.exe", "/C", command)
	}
	return exec.Command("sh", "-c", command)
}
