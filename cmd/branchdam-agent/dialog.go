package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/ncruces/zenity"
)

// dialogExitCanceled is returned by `dialog` when the user dismissed a
// prompt (Cancel, window close) rather than the dialog failing to render
// at all -- callers re-exec'ing this subcommand (runTrayCmd's startup-error
// notification, the first-run setup wizard) need to tell "the operator
// said no" apart from "nothing showed up at all" (dialogExitFailed).
const (
	dialogExitOK          = 0
	dialogExitFailed      = 1
	dialogExitCanceled    = 2
	dialogExitExtraButton = 3
)

// dialogFuncs is the subset of github.com/ncruces/zenity's surface
// runDialogCmd needs, indirected the same way cmd/branchdam-agent's other
// subcommands substitute fakes for real OS/network calls (lookPathFunc,
// runVersionFunc, helloCaller) -- a real dialog can't be driven from a
// headless test, but the flag parsing, exit-code mapping, and stdout
// contract around it can be.
type dialogFuncs struct {
	showError func(title, message string) error
	entry     func(title, message, defaultText string, hidden bool) (string, error)
	directory func(title string) (string, error)
	// file selects an existing FILE (never a directory -- that's what
	// makes this distinct from directory above), optionally filtered to
	// patterns (e.g. "*.lrcat", "*.json"). Needed for the integrations
	// menu's catalog and node-index pickers (issue #58) -- neither is a
	// directory, which directory's zenity.Directory() option structurally
	// cannot return.
	file func(title, defaultPath string, patterns []string) (string, error)
	// question shows a Yes/No (or OK/Cancel on Windows / native macOS
	// backends) confirmation dialog and returns whether the operator
	// accepted (nil err = OK; zenity.ErrCanceled = Cancel/window-close;
	// zenity.ErrExtraButton = Extra button; any other err = the dialog
	// didn't render). okLabel, cancelLabel, and extraButton are
	// optional custom button labels (empty string keeps the platform
	// default) and feed zenity.OKLabel / zenity.CancelLabel /
	// zenity.ExtraButton.
	question func(title, message, okLabel, cancelLabel, extraButton string) error
	// notify shows an OS desktop notification (toast/bubble/notification
	// banner) with title and message. Pass an empty title to skip
	// zenity.Title entirely; zenity.Notify on some Linux backends
	// accepts an empty title while others ignore it.
	notify func(title, message string) error
}

// realDialogFuncs is dialogFuncs backed by the real zenity package: Win32
// native dialogs on Windows, AppleScript (osascript) on macOS, and the
// zenity/matedialog/qarma binary on Linux -- none of which is cgo, so this
// import doesn't change any of this repo's cross-compile properties (see
// CLAUDE.md's build-windows/build-darwin invariants). The `dialog`
// subcommand -- not internal/tray -- owns this import, since the tray's
// systray-based menu (windows/darwin only) and this headless subcommand
// have no reason to share a build tag: `dialog` compiles and could in
// principle run on Linux too, wherever a zenity backend is installed.
var realDialogFuncs = dialogFuncs{
	showError: func(title, message string) error {
		return zenity.Error(message, zenity.Title(title))
	},
	entry: func(title, message, defaultText string, hidden bool) (string, error) {
		opts := []zenity.Option{zenity.Title(title)}
		if defaultText != "" {
			opts = append(opts, zenity.EntryText(defaultText))
		}
		if hidden {
			opts = append(opts, zenity.HideText())
		}
		return zenity.Entry(message, opts...)
	},
	directory: func(title string) (string, error) {
		return zenity.SelectFile(zenity.Title(title), zenity.Directory())
	},
	file: func(title, defaultPath string, patterns []string) (string, error) {
		opts := []zenity.Option{zenity.Title(title)}
		if defaultPath != "" {
			opts = append(opts, zenity.Filename(defaultPath))
		}
		if len(patterns) > 0 {
			opts = append(opts, zenity.FileFilters{
				{Name: "Supported files", Patterns: patterns, CaseFold: true},
			})
		}
		// No zenity.Directory() here -- that option is what distinguishes
		// this from the directory func above.
		return zenity.SelectFile(opts...)
	},
	question: func(title, message, okLabel, cancelLabel, extraButton string) error {
		var opts []zenity.Option
		if title != "" {
			opts = append(opts, zenity.Title(title))
		}
		if okLabel != "" {
			opts = append(opts, zenity.OKLabel(okLabel))
		}
		if cancelLabel != "" {
			opts = append(opts, zenity.CancelLabel(cancelLabel))
		}
		if extraButton != "" {
			opts = append(opts, zenity.ExtraButton(extraButton))
		}
		opts = append(opts, zenity.WarningIcon)
		return zenity.Question(message, opts...)
	},
	notify: func(title, message string) error {
		var opts []zenity.Option
		if title != "" {
			opts = append(opts, zenity.Title(title))
		}
		return zenity.Notify(message, opts...)
	},
}

// runDialogCmd implements the hidden `branchdam-agent dialog` subcommand --
// deliberately omitted from usage()'s printed subcommand list, since it
// exists only to be re-exec'd by this same binary (runTrayCmd's
// startup-error notification, and the first-run setup wizard in init.go),
// never invoked directly by an operator. Re-exec'ing into a fresh process
// per dialog, rather than calling zenity in-process from the tray, sidesteps
// two platform-specific unknowns neither of which can be verified from this
// (Linux) development environment: whether a Win32 dialog renders correctly
// from a `-H windowsgui`-linked process before systray's own message pump
// has started, and whatever state a macOS `.app` launched by launchd
// assumes about the calling process. See CLAUDE.md's self-update/tray
// invariants for the general pattern (isolate the platform-uncertain part
// in its own process) and issue #30 for why this exists at all.
func runDialogCmd(args []string, dlg dialogFuncs) int {
	fs := flag.NewFlagSet("dialog", flag.ContinueOnError)
	kind := fs.String("kind", "", "dialog kind: error, entry, password, directory, file, question, or notify")
	title := fs.String("title", "branchDAM Agent", "dialog title")
	message := fs.String("message", "", "dialog body text (error/entry/password/question/notify only)")
	// -default is reused as file's pre-filled path -- unlike password,
	// where a secret has no business appearing in a subprocess's argv, a
	// filesystem path is not a secret.
	defaultText := fs.String("default", "", "pre-filled text (entry/password only) or path (file only)")
	patterns := fs.String("patterns", "", "comma-separated filename patterns for -kind file (e.g. \"*.lrcat,*.json\"); empty means no filter")
	okLabel := fs.String("ok-label", "", "custom OK button label (question only)")
	cancelLabel := fs.String("cancel-label", "", "custom Cancel button label (question only)")
	extraButton := fs.String("extra-button", "", "extra button label (question only)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	switch *kind {
	case "error":
		if err := dlg.showError(*title, *message); err != nil {
			fmt.Fprintf(os.Stderr, "branchdam-agent dialog: %v\n", err)
			return dialogExitFailed
		}
		return dialogExitOK

	case "entry", "password":
		value, err := dlg.entry(*title, *message, *defaultText, *kind == "password")
		return reportPromptResult(value, err)

	case "directory":
		value, err := dlg.directory(*title)
		return reportPromptResult(value, err)

	case "file":
		value, err := dlg.file(*title, *defaultText, splitPatterns(*patterns))
		return reportPromptResult(value, err)

	case "question":
		// Issue #108 / E3 #S2-14: yes/no confirmation prompt for the
		// tray's destructive-action gate, and issue #79 card import confirmation.
		if err := dlg.question(*title, *message, *okLabel, *cancelLabel, *extraButton); err != nil {
			if errors.Is(err, zenity.ErrCanceled) {
				return dialogExitCanceled
			}
			if errors.Is(err, zenity.ErrExtraButton) {
				return dialogExitExtraButton
			}
			fmt.Fprintf(os.Stderr, "branchdam-agent dialog: %v\n", err)
			return dialogExitFailed
		}
		return dialogExitOK

	case "notify", "notification":
		if err := dlg.notify(*title, *message); err != nil {
			fmt.Fprintf(os.Stderr, "branchdam-agent dialog: %v\n", err)
			return dialogExitFailed
		}
		return dialogExitOK

	default:
		fmt.Fprintf(os.Stderr, "branchdam-agent dialog: -kind must be one of error, entry, password, directory, file, question, notify (got %q)\n", *kind)
		return 2
	}
}

// splitPatterns parses -patterns' comma-separated form, trimming
// whitespace and dropping empty entries -- mirrors
// runLuminarSyncCmd's own -derivative-suffixes splitting so both
// comma-separated flags in this binary behave identically. An empty or
// all-whitespace input returns nil (no filter), never a single
// empty-string pattern.
func splitPatterns(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// reportPromptResult prints value to stdout (the only channel a re-exec'd
// caller reads back) on success, and maps zenity.ErrCanceled to
// dialogExitCanceled so a caller can tell "the operator said no" apart
// from dialogExitFailed's "the dialog didn't even render."
func reportPromptResult(value string, err error) int {
	if err != nil {
		if errors.Is(err, zenity.ErrCanceled) {
			return dialogExitCanceled
		}
		fmt.Fprintf(os.Stderr, "branchdam-agent dialog: %v\n", err)
		return dialogExitFailed
	}
	fmt.Println(value)
	return dialogExitOK
}
