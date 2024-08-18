package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"

	"github.com/filebrowser/filebrowser/v2/settings"
	"github.com/filebrowser/filebrowser/v2/users"
	"github.com/redis/go-redis/v9"
)

const FileBrowserQueue = "fbq"

// filterEmptyParts removes empty strings from the command slice.
func filterEmptyParts(command []string) []string {
	filteredCommand := command[:0]
	for _, part := range command {
		if part != "" {
			filteredCommand = append(filteredCommand, part)
		}
	}
	return filteredCommand
}

// Runner is a commands runner.
type Runner struct {
	Enabled     bool
	RedisClient *redis.Client
	*settings.Settings
}

// RunHook runs the hooks for the before and after event.
func (r *Runner) RunHook(fn func() error, evt, path, dst string, user *users.User) error {
	path = user.FullPath(path)
	dst = user.FullPath(dst)

	if r.Enabled {
		// these should not be queued, if there is some blocking process that we need
		// to do before executing fn(), then we can't queue it in redis,
		// it needs to be done immediately.
		if val, ok := r.Commands["before_"+evt]; ok {
			for _, command := range val {
				err := r.exec(command, "before_"+evt, path, dst, user)
				if err != nil {
					return err
				}
			}
		}
	}

	err := fn()
	if err != nil {
		return err
	}

	if r.Enabled {
		// queue here
		if val, ok := r.Commands["after_"+evt]; ok {
			for _, command := range val {
				job := struct {
					Command     string `json:"command"`
					Event       string `json:"event"`
					Path        string `json:"path"`
					Destination string `json:"destination"`
					UserName    string `json:"username"`
					UserScope   string `json:"user_scope"`
				}{
					Command:     command,
					Event:       "after_" + evt,
					Path:        path,
					Destination: dst,
					UserName:    user.Username,
					UserScope:   user.Scope,
				}

				jobBytes, err := json.Marshal(job)
				if err != nil {
					return err
				}

				res := r.RedisClient.LPush(context.Background(), FileBrowserQueue, jobBytes)
				if res.Err() != nil {
					return fmt.Errorf("failed to queue job: %w", res.Err())
				}
			}
		}
	}

	return nil
}

func (r *Runner) exec(raw, evt, path, dst string, user *users.User) error {
	blocking := true

	raw = strings.TrimSpace(raw)

	if strings.HasSuffix(raw, "&") {
		log.Printf("[DEBUG] non blocking")
		blocking = false
		raw = strings.TrimSpace(strings.TrimSuffix(raw, "&"))
	}

	command, err := ParseCommand(r.Settings, raw)
	if err != nil {
		return err
	}

	envMapping := func(key string) string {
		switch key {
		case "FILE":
			return path
		case "SCOPE":
			return user.Scope
		case "TRIGGER":
			return evt
		case "USERNAME":
			return user.Username
		case "DESTINATION":
			return dst
		default:
			return os.Getenv(key)
		}
	}
	for i, arg := range command {
		if i == 0 {
			continue
		}
		command[i] = os.Expand(arg, envMapping)
	}
	command = filterEmptyParts(command)

	cmd := exec.Command(command[0], command[1:]...) //nolint:gosec
	cmd.Env = append(os.Environ(), fmt.Sprintf("FILE=%s", path))
	cmd.Env = append(cmd.Env, fmt.Sprintf("SCOPE=%s", user.Scope)) //nolint:gocritic
	cmd.Env = append(cmd.Env, fmt.Sprintf("TRIGGER=%s", evt))
	cmd.Env = append(cmd.Env, fmt.Sprintf("USERNAME=%s", user.Username))
	cmd.Env = append(cmd.Env, fmt.Sprintf("DESTINATION=%s", dst))

	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if !blocking {
		log.Printf("[INFO] Nonblocking Command: \"%s\"", strings.Join(command, " "))
		defer func() {
			go func() {
				err := cmd.Wait()
				if err != nil {
					log.Printf("[INFO] Nonblocking Command \"%s\" failed: %s", strings.Join(command, " "), err)
				}
			}()
		}()
		return cmd.Start()
	}

	log.Printf("[INFO] Blocking Command: \"%s\"", strings.Join(command, " "))
	return cmd.Run()
}
