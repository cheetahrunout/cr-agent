// Package ui holds the agent's few native dialogs: the pairing code, the Hugging Face token, the
// model folder and Allow/Deny for a download. They are the agent's own windows (ruling B14); the
// texts are built here from the agent's own data, never from hub strings.
package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

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
	// Every file is listed: Allow covers exactly these (finding M2).
	b.WriteString(strings.Join(p.Files, "\n"))
	b.WriteString("\n" + HumanSize(p.SizeBytes) + " · commit " + shortCommit(p.Commit) + "\n\n")
	b.WriteString("Allow it only if you asked for this in the hub.")
	return b.String()
}

func shortCommit(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	return c
}

// safe makes a file name safe to show, at full length: no control or formatting characters (a
// right-to-left mark can make one name read as another).
func safe(name string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return '?'
		}
		return r
	}, name)
}

// clean is safe, shortened for a text that is not an approval. The end of the name stays.
func clean(name string) string { return shorten(safe(name), 80) }

// shorten keeps a name's start and end, where a model file says what it is: its quantization.
func shorten(name string, width int) string {
	r := []rune(name)
	if len(r) <= width {
		return name
	}
	tail := width * 2 / 3
	return string(r[:width-tail-1]) + "…" + string(r[len(r)-tail:])
}

// rowWidth is what fits in a row of zenity's list window, which has a fixed size on Windows.
const rowWidth = 28

// FoundRow is one row of the Add model files list: size and name, shortened to fit. The confirmation
// after it shows every full name.
func FoundRow(f models.Found) string {
	if len(f.Files) > 1 {
		return fmt.Sprintf("%s · %s (%d parts)", HumanSize(f.Size), shorten(safe(models.FileStem(f.Key)), rowWidth-10), len(f.Files))
	}
	return HumanSize(f.Size) + " · " + shorten(safe(f.Key), rowWidth)
}

// NothingToAddText is what the host reads when nothing can be added.
func NothingToAddText(s models.Scan) string {
	text := "There are no new model files to add in\n" + s.Dir + "\n\nCopy GGUF files into that folder, or into one folder inside it, and choose this again."
	if len(s.Refused) > 0 {
		text += "\n\n" + RefusedText(s)
	}
	return text
}

// RefusedText says which files cannot be added, and why. Only a file's own name comes from outside the agent.
func RefusedText(s models.Scan) string {
	var b strings.Builder
	b.WriteString("These cannot be added:\n")
	for i, r := range s.Refused {
		if i == 6 {
			fmt.Fprintf(&b, "and %d more\n", len(s.Refused)-i)
			break
		}
		b.WriteString(clean(r.Name) + ": " + r.Why + "\n")
	}
	return b.String()
}

// listRows pairs each row with its key; two files that shorten to one row are told apart by a number.
func listRows(s models.Scan) ([]string, map[string]string) {
	rows := make([]string, 0, len(s.Found))
	key := map[string]string{}
	for _, f := range s.Found {
		r := FoundRow(f)
		for n := 2; key[r] != ""; n++ {
			r = fmt.Sprintf("%s (%d)", FoundRow(f), n)
		}
		rows = append(rows, r)
		key[r] = f.Key
	}
	return rows, key
}

// PickModelFiles asks which files to add and returns their keys. Nothing is chosen in advance.
// zenity's list window has a fixed size on Windows, so the options for its size would do nothing.
func PickModelFiles(s models.Scan) ([]string, bool) {
	rows, key := listRows(s)
	sel, err := zenity.ListMultiple("Files to add (Ctrl+click for several):", rows, zenity.Title(title+" · Add model files"),
		zenity.OKLabel("Next"), zenity.CancelLabel("Cancel"), zenity.DisallowEmpty())
	if err != nil || len(sel) == 0 {
		return nil, false
	}
	out := make([]string, 0, len(sel))
	for _, r := range sel {
		if k, ok := key[r]; ok {
			out = append(out, k)
		}
	}
	return out, len(out) > 0
}

// ConfirmAddText names in full what the host is about to approve: this click is the approval M3 asks for.
func ConfirmAddText(s models.Scan, keys []string) string {
	var b strings.Builder
	b.WriteString("Add these files, so that the hub can use them on this PC?\n\n")
	for _, k := range keys {
		for _, f := range s.Found {
			if f.Key != k {
				continue
			}
			b.WriteString(safe(f.Key) + " · " + HumanSize(f.Size))
			if len(f.Files) > 1 {
				fmt.Fprintf(&b, " · %d parts", len(f.Files))
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("\nAdd only files you trust: the engine will load them.")
	return b.String()
}

// ImportResultText is the summary after an import.
func ImportResultText(results []models.ImportResult) string {
	var added, failed []string
	for _, r := range results {
		if r.Err == "" {
			added = append(added, safe(r.Key))
		} else {
			failed = append(failed, safe(r.Key)+": "+r.Err)
		}
	}
	var b strings.Builder
	if len(added) > 0 {
		b.WriteString("Added, and now listed for this PC in the hub:\n" + strings.Join(added, "\n") + "\n")
	}
	if len(failed) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("Not added:\n" + strings.Join(failed, "\n") + "\n")
	}
	return b.String()
}

// Progress is the window that shows an import's hashing. Closing it cancels the import.
type Progress struct{ dlg zenity.ProgressDialog }

// NewProgress opens the window; onCancel runs if the host closes it. A window that cannot open is no
// reason to stop: the hash goes on without one.
func NewProgress(onCancel func()) *Progress {
	dlg, err := zenity.Progress(zenity.Title(title+" · Add model files"), zenity.MaxValue(100))
	if err != nil {
		return &Progress{}
	}
	go func() {
		<-dlg.Done()
		onCancel()
	}()
	return &Progress{dlg: dlg}
}

// Set shows the file being checked and how far it all got.
func (p *Progress) Set(file string, done, total int64) {
	if p.dlg == nil {
		return
	}
	_ = p.dlg.Text("Checking " + clean(file))
	if total > 0 {
		_ = p.dlg.Value(int(float64(done) / float64(total) * 99))
	}
}

// Close removes the window.
func (p *Progress) Close() {
	if p.dlg != nil {
		_ = p.dlg.Close()
	}
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
