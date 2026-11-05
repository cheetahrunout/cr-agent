// Package ui holds the agent's few native dialogs: the pairing code, the Hugging Face token, the
// model folder and Allow/Deny for a download. They are the agent's own windows (ruling B14); the
// texts are built here from the agent's own data, never from hub strings.
package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ncruces/zenity"

	"github.com/cheetahrunout/cr-agent/internal/models"
)

const title = "CR Agent"

// AskPairingCode asks for the code or pairing string from the hub page.
func AskPairingCode() (string, bool) {
	s, err := zenity.Entry("Paste the pairing code from the hub (Add a machine).\nIt looks like K7M2-9QWD-X3.",
		zenity.Title(title+" · Pair with the hub"), zenity.OKLabel("Pair"), zenity.Width(380))
	return strings.TrimSpace(s), err == nil
}

// AskHFToken asks for a Hugging Face token without echo. An empty answer means "remove the token".
func AskHFToken() (string, bool) {
	_, tok, err := zenity.Password(zenity.Title(title+" · Hugging Face token"), zenity.OKLabel("Save"))
	return strings.TrimSpace(tok), err == nil
}

// PickFolder asks for the model folder.
func PickFolder(current string) (string, bool) {
	dir, err := zenity.SelectFile(zenity.Title(title+" · Model folder"), zenity.Directory(), zenity.Filename(current))
	return dir, err == nil && dir != ""
}

// Info shows a message.
func Info(text string) { _ = zenity.Info(text, zenity.Title(title)) }

// Error shows a problem.
func Error(text string) { _ = zenity.Error(text, zenity.Title(title)) }

// Confirm asks a yes/no question; the default is no.
func Confirm(text, yes string) bool {
	return zenity.Question(text, zenity.Title(title), zenity.OKLabel(yes), zenity.CancelLabel("Cancel"), zenity.DefaultCancel()) == nil
}

// HumanSize formats bytes as the download dialog shows them.
func HumanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d KB", (n+1023)/1024)
	}
}

// ApprovalText is the Allow/Deny question. Repo, files and size come from the agent's own
// Hugging Face lookup (§9.2); a new commit of a repo allowed before says so (ruling B13).
func ApprovalText(p models.Prompt) string {
	var b strings.Builder
	if p.AllowedBefore {
		b.WriteString("Download a new version of a repo you allowed before?\n\n")
	} else {
		b.WriteString("Download a model from a publisher that is not on the built-in list?\n\n")
	}
	b.WriteString(p.Repo + "\n")
	files := p.Files
	if len(files) > 3 {
		files = append(append([]string{}, files[:3]...), fmt.Sprintf("and %d more files", len(p.Files)-3))
	}
	b.WriteString(strings.Join(files, "\n"))
	b.WriteString(" · " + HumanSize(p.SizeBytes) + "\n\n")
	b.WriteString("Allow it only if you asked for this in the hub.")
	return b.String()
}

// ApprovalLabel is the short form for the tray menu and the notification.
func ApprovalLabel(p models.Prompt) string { return p.Repo + " (" + HumanSize(p.SizeBytes) + ")" }

// AllowDownload shows the Allow/Deny dialog. answered is false when the dialog went away because ctx
// ended (the 10-minute deadline) and nobody chose.
func AllowDownload(ctx context.Context, p models.Prompt) (allow, answered bool) {
	err := zenity.Question(ApprovalText(p), zenity.Title(title+" · Download"), zenity.OKLabel("Allow"), zenity.CancelLabel("Deny"),
		zenity.DefaultCancel(), zenity.WarningIcon, zenity.Context(ctx))
	switch {
	case err == nil:
		return true, true
	case errors.Is(err, zenity.ErrCanceled) && ctx.Err() == nil:
		return false, true
	}
	return false, false
}
