package exec

import "context"

// ReadOnly reports whether an invocation only observes the world. Under
// --dry-run these still run; everything else is printed and skipped.
type ReadOnly func(Opts) bool

// DryRun wraps a Runner so that mutating commands are announced instead of
// executed. Read-only commands pass straight through, because a dry run that
// cannot inspect the repository cannot describe what it would do.
type DryRun struct {
	Inner    Runner
	IsRead   ReadOnly
	Announce func(cmdline string)
}

var _ Runner = (*DryRun)(nil)

// Run executes read-only commands and announces mutating ones.
func (d *DryRun) Run(ctx context.Context, opts Opts) (Result, error) {
	if d.IsRead != nil && d.IsRead(opts) {
		return d.Inner.Run(ctx, opts)
	}
	if d.Announce != nil {
		d.Announce(Command(opts))
	}
	return Result{}, nil
}

// StartDetached announces the spawn without performing it.
func (d *DryRun) StartDetached(_ context.Context, opts Opts) error {
	if d.Announce != nil {
		d.Announce(Command(opts))
	}
	return nil
}

// LookPath is read-only and always delegates.
func (d *DryRun) LookPath(name string) (string, error) { return d.Inner.LookPath(name) }
