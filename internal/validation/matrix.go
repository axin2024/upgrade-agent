package validation

import (
	"context"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/axin2024/upgrade-agent/internal/snapshot"
)

type MatrixInput struct {
	Provider      snapshot.Snapshot
	Consumer      snapshot.Snapshot
	ProviderTest  string
	ProviderRoute string
	ConsumerTest  string
	Language      string
}

type MatrixResult struct {
	Observation
	Image         string `json:"image"`
	Command       string `json:"command"`
	Provider      string `json:"provider_snapshot"`
	Consumer      string `json:"consumer_snapshot"`
	DependencySHA string `json:"dependency_sha256"`
	Output        string `json:"response_excerpt,omitempty"`
	Evidence      string `json:"evidence_sha256"`
}

type MatrixRunner interface {
	RunMatrix(context.Context, MatrixInput) MatrixResult
}

func (d Docker) RunMatrix(ctx context.Context, in MatrixInput) MatrixResult {
	result := MatrixResult{Observation: Observation{State: "unavailable", ExitCode: -1}, Image: d.Identity(),
		Provider: in.Provider.Digest, Consumer: in.Consumer.Digest, DependencySHA: MatrixDependencySHA(in)}
	result.Command = "provider PHP regression and " + in.Language + " consumer contract test"
	if err := validateMatrixInput(in); err != nil {
		result.Observation.State, result.Output = "rejected", "rehearsal test or router is absent from the approved snapshot"
		return SealMatrixResult(result)
	}
	dir, err := os.MkdirTemp("", "migration-matrix-")
	if err != nil {
		return SealMatrixResult(result)
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0755); err != nil {
		return SealMatrixResult(result)
	}
	for name, source := range map[string]snapshot.Snapshot{"provider": in.Provider, "consumer": in.Consumer} {
		destination := filepath.Join(dir, name)
		if err := os.Mkdir(destination, 0755); err != nil {
			return SealMatrixResult(result)
		}
		if err := source.Materialize(destination, nil); err != nil {
			return SealMatrixResult(result)
		}
	}
	result.Command = "php " + in.ProviderTest + " ; " + in.Language + " " + in.ConsumerTest
	if in.ProviderRoute != "" {
		result.Command += " (provider router " + in.ProviderRoute + ")"
	}
	observed, output, _ := d.runIsolated(ctx, dir, 32<<10, true, "sh", "-eu", "-c", matrixScript,
		"migration-matrix", in.ProviderTest, in.ProviderRoute, in.ConsumerTest, in.Language, "./"+path.Dir(in.ConsumerTest))
	result.Observation = observed
	if observed.State == "failed" && observed.ExitCode == 78 {
		result.Observation.State = "dependency-unavailable"
	}
	if observed.State != "passed" {
		output = output[:min(len(output), 2048)]
		result.Output = strings.TrimSpace(strings.ToValidUTF8(string(output), "�"))
	}
	return SealMatrixResult(result)
}

func MatrixDependencySHA(in MatrixInput) string {
	var joined strings.Builder
	for _, repo := range []snapshot.Snapshot{in.Provider, in.Consumer} {
		for _, name := range []string{"composer.json", "composer.lock", "go.mod", "go.sum"} {
			if file, ok := repo.Find(name); ok {
				joined.WriteString(name + ":" + file.SHA256 + "\n")
			}
		}
		joined.WriteByte(0)
	}
	return snapshot.Digest([]byte(joined.String()))
}

func SealMatrixResult(result MatrixResult) MatrixResult {
	result.Evidence = ""
	data, _ := json.Marshal(result)
	result.Evidence = snapshot.Digest(data)
	return result
}

func validateMatrixInput(in MatrixInput) error {
	for _, candidate := range []struct {
		repo snapshot.Snapshot
		name string
	}{{in.Provider, in.ProviderTest}, {in.Consumer, in.ConsumerTest}} {
		if err := candidate.repo.Verify(); err != nil {
			return err
		}
		if _, ok := candidate.repo.Find(candidate.name); !ok || !snapshot.SafePath(candidate.name) {
			return os.ErrNotExist
		}
	}
	if in.ProviderRoute != "" {
		if _, ok := in.Provider.Find(in.ProviderRoute); !ok || !snapshot.SafePath(in.ProviderRoute) || !strings.HasSuffix(in.ProviderRoute, ".php") {
			return os.ErrNotExist
		}
	}
	if !strings.HasSuffix(in.ProviderTest, ".php") || in.Language != "php" && in.Language != "go" ||
		in.Language == "php" && !strings.HasSuffix(in.ConsumerTest, ".php") ||
		in.Language == "go" && !strings.HasSuffix(in.ConsumerTest, "_test.go") || !utf8.ValidString(in.ProviderTest) {
		return os.ErrInvalid
	}
	return nil
}

const matrixScript = `
export PATH="/usr/local/go/bin:$PATH" GOCACHE=/scratch/go-cache GOPATH=/scratch/go GOPROXY=off
export GOSUMDB=off GOTOOLCHAIN=local GOENV=off CGO_ENABLED=0 GOTMPDIR=/scratch
export UPGRADE_PROVIDER_ROOT=/scratch/provider UPGRADE_CONSUMER_ROOT=/scratch/consumer
export UPGRADE_PROVIDER_URL=http://127.0.0.1:8080
cp -R /work/provider /work/consumer /scratch/
chmod -R u+w /scratch/provider /scratch/consumer
install_php() {
    if [ -f composer.json ]; then
        if [ ! -f composer.lock ]; then echo "Composer lock is required"; exit 78; fi
        php /opt/upgrade-agent/composer.phar install --no-ansi --no-interaction --no-plugins --no-scripts --no-progress || exit 78
    fi
}
exec 2>&1
cd "$UPGRADE_PROVIDER_ROOT"
install_php
php -d memory_limit=96M "./$1" || exit 1
if [ -n "$2" ]; then
    php -S 127.0.0.1:8080 "./$2" > /tmp/provider-http.log 2>&1 &
    server=$!
    trap 'kill "$server" 2>/dev/null || true' EXIT
    php -r 'for ($i = 0; $i < 100; $i++) { $c = @fsockopen("127.0.0.1", 8080, $e, $s, 0.1); if ($c) { fclose($c); exit(0); } usleep(50000); } exit(1);' || { cat /tmp/provider-http.log; exit 1; }
fi
cd "$UPGRADE_CONSUMER_ROOT"
case "$4" in
    php)
        install_php
        php -d memory_limit=96M "./$3" || exit 1
        ;;
    go)
        if [ ! -f go.mod ]; then echo "go.mod is required"; exit 78; fi
        command -v go >/dev/null || exit 78
        go list -mod=readonly -deps -test "$5" >/tmp/go-deps.log 2>&1 || {
            cat /tmp/go-deps.log
            if grep -Eq 'GOPROXY=off|missing go.sum entry|no required module' /tmp/go-deps.log; then exit 78; fi
            exit 1
        }
        go test -mod=readonly -count=1 -p=1 "$5" || exit 1
        ;;
    *) exit 64 ;;
esac
`
