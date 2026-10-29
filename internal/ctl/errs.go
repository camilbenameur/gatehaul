package ctl

import (
	"errors"

	"github.com/camilbenameur/gatehaul/internal/job"
	"github.com/camilbenameur/gatehaul/internal/runner"
)

func asAmbiguous(err error, target **job.AmbiguousError) bool { return errors.As(err, target) }

func isNotFound(err error) bool { return errors.Is(err, job.ErrNotFound) }

func isOutcomeUnknown(err error) bool { return errors.Is(err, runner.ErrOutcomeUnknown) }
