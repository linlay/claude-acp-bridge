package claudecli

import "strings"

type RuntimeOptions struct {
	Model                      string
	Effort                     string
	PermissionMode             string
	DangerouslySkipPermissions bool
}

func ApplyRuntimeArgs(base []string, opts RuntimeOptions) []string {
	args := append([]string(nil), base...)
	if model := strings.TrimSpace(opts.Model); model != "" {
		args = append(args, "--model", model)
	}
	if effort := strings.TrimSpace(opts.Effort); effort != "" {
		args = append(args, "--effort", effort)
	}
	if mode := strings.TrimSpace(opts.PermissionMode); mode != "" && !HasArg(args, "--permission-mode") {
		args = append(args, "--permission-mode", mode)
	}
	if opts.DangerouslySkipPermissions && !HasArg(args, "--dangerously-skip-permissions") {
		args = append(args, "--dangerously-skip-permissions")
	}
	return args
}

func WithoutPermissionBypass(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--permission-mode":
			if i+1 < len(args) {
				i++
			}
			continue
		case strings.HasPrefix(arg, "--permission-mode="):
			continue
		case arg == "--dangerously-skip-permissions":
			continue
		default:
			out = append(out, arg)
		}
	}
	return out
}

func HasArg(args []string, name string) bool {
	for _, arg := range args {
		if arg == name || strings.HasPrefix(arg, name) {
			return true
		}
	}
	return false
}
