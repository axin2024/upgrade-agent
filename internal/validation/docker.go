package validation

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/axin2024/upgrade-agent/internal/snapshot"
)

// Docker uses a locally built, immutable Linux image containing PHP and Composer.
type Docker struct {
	binary  string
	image   string
	timeout time.Duration
}

var imageID = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func NewDocker(binary, image string, timeout time.Duration) (Docker, error) {
	if !filepath.IsAbs(binary) || !imageID.MatchString(image) || timeout < time.Second || timeout > 2*time.Minute {
		return Docker{}, errors.New("configure an absolute Docker path, immutable local image ID, and 1–120s timeout")
	}
	return Docker{binary: binary, image: image, timeout: timeout}, nil
}

func (d Docker) Identity() string { return "docker/" + d.image }

func (d Docker) Check(ctx context.Context, directory, kind, file string) Observation {
	var command []string
	switch kind {
	case "php-lint", "php-test":
		if !snapshot.SafePath(file) || !strings.HasSuffix(file, ".php") {
			return Observation{State: "unavailable", ExitCode: -1}
		}
		if kind == "php-lint" {
			command = []string{"php", "-d", "memory_limit=128M", "-l", "/work/" + file}
		} else {
			command = []string{"sh", "-eu", "-c", `
if [ ! -f composer.lock ]; then
    exec php -d memory_limit=128M "/work/$1"
fi
mkdir /tmp/test
cp -R /work/. /tmp/test
cd /tmp/test
php /opt/upgrade-agent/composer.phar install --no-ansi --no-interaction --no-plugins --no-scripts --no-progress || exit 78
exec php -d memory_limit=128M "$1"
`, "upgrade-php-test", file}
		}
	case "composer-lock":
		command = []string{"sh", "-eu", "-c", `
php /opt/upgrade-agent/composer.phar validate --check-lock --no-check-publish --no-ansi --no-interaction --no-plugins --no-scripts
php /opt/upgrade-agent/composer.phar install --dry-run --no-ansi --no-interaction --no-plugins --no-scripts --no-progress
`}
	default:
		return Observation{State: "unavailable", ExitCode: -1}
	}
	observed, _, _ := d.run(ctx, directory, 256<<10, command...)
	if kind == "php-test" && observed.State == "failed" && observed.ExitCode == 78 {
		observed.State = "unavailable"
	}
	return observed
}

func (d Docker) Regenerate(ctx context.Context, directory string) (string, error) {
	observed, lock, err := d.run(ctx, directory, 1<<20, "sh", "-eu", "-c", `
mkdir /tmp/solve
cp -R /work/. /tmp/solve
cd /tmp/solve
php /opt/upgrade-agent/composer.phar update --no-install --no-ansi --no-interaction --no-plugins --no-scripts --no-progress 1>&2
cat composer.lock
`)
	if err != nil || observed.State != "passed" {
		return "", fmt.Errorf("Composer lock generation %s (exit %d, output %s)", observed.State, observed.ExitCode, observed.OutputSHA256)
	}
	return string(lock), nil
}

func (d Docker) run(parent context.Context, directory string, outputLimit int, command ...string) (Observation, []byte, error) {
	start := time.Now()
	state := Observation{State: "unavailable", ExitCode: -1}
	if strings.ContainsAny(directory, ",\x00") || !filepath.IsAbs(directory) {
		return state, nil, errors.New("invalid workspace mount path")
	}
	ctx, cancel := context.WithTimeout(parent, d.timeout)
	defer cancel()
	name := "upgrade-check-" + snapshot.Digest([]byte(directory + start.String()))[:24]
	flags := []string{"run", "--rm", "--name", name, "--pull", "never", "--network", "none",
		"--read-only", "--user", "65534:65534", "--pids-limit", "64", "--memory", "256m", "--cpus", "1",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--log-driver", "none",
		"--tmpfs", "/tmp:rw,nosuid,nodev,noexec,size=64m,mode=1777",
		"--env", "HOME=/tmp", "--env", "COMPOSER_HOME=/tmp/composer",
		"--env", "COMPOSER_CACHE_DIR=/opt/upgrade-agent/composer-cache",
		"--env", "COMPOSER_CACHE_READ_ONLY=1", "--env", "COMPOSER_DISABLE_NETWORK=1",
		"--mount", "type=bind,source=" + directory + ",target=/work,readonly",
		"--workdir", "/work", "--entrypoint", command[0], d.image}
	process := exec.CommandContext(ctx, d.binary, append(flags, command[1:]...)...)
	process.WaitDelay = time.Second
	stdout, stderr := &limitedBuffer{limit: outputLimit}, &limitedBuffer{limit: 256 << 10}
	process.Stdout, process.Stderr = stdout, stderr
	err := process.Run()
	state.Milliseconds = time.Since(start).Milliseconds()
	state.OutputBytes = stdout.Len() + stderr.Len()
	state.OutputSHA256 = snapshot.Digest(append(append([]byte{}, stdout.Bytes()...), stderr.Bytes()...))
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		state.State = "cancelled"
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			state.State = "timeout"
		}
		cleanup, stop := context.WithTimeout(context.Background(), 4*time.Second)
		remove := exec.CommandContext(cleanup, d.binary, "rm", "-f", name)
		remove.Stdout, remove.Stderr = io.Discard, io.Discard
		_ = remove.Run()
		stop()
	case stdout.truncated || stderr.truncated:
		state.State = "output-limit"
	case err == nil:
		state.State, state.ExitCode = "passed", 0
	case errors.As(err, &exit):
		state.ExitCode = exit.ExitCode()
		if state.ExitCode < 125 {
			state.State = "failed"
		} else if state.ExitCode == 137 {
			state.State = "resource-limit"
		}
	}
	return state, stdout.Bytes(), err
}

type limitedBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	length := len(p)
	if len(p) > b.limit-b.Len() {
		p = p[:b.limit-b.Len()]
		b.truncated = true
	}
	_, _ = b.Buffer.Write(p)
	return length, nil
}
