package validation

import (
	"context"
	"errors"
)

type Unavailable struct{}

func (Unavailable) Identity() string { return "unconfigured" }

func (Unavailable) Check(context.Context, string, string, string) Observation {
	return Observation{State: "unavailable", ExitCode: -1}
}

func (Unavailable) Regenerate(context.Context, string) (string, error) {
	return "", errors.New("validation backend unavailable")
}
